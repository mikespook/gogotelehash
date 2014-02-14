package telehash

import (
	"time"
)

type cmd_peer_get struct {
	hashname Hashname
	make_new bool
	peer     *Peer
}

func (cmd *cmd_peer_get) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	for _, dht := range sw.dhts {
		cmd.peer = dht.GetPeer(cmd.hashname)
		if cmd.peer != nil {
			return nil
		}
	}
	if cmd.make_new {
		cmd.peer = make_peer(sw, cmd.hashname)

		for _, hook := range sw.hook_new_peer {
			hook.OnNewPeer(cmd.peer)
		}
	}
	return nil
}

type cmd_line_get struct {
	hashname Hashname
	line     *line_t
}

func (cmd *cmd_line_get) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	cmd.line = sw.lines[cmd.hashname]
	return nil
}

type cmd_shutdown struct {
}

func (cmd *cmd_shutdown) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	sw.terminating = true

	sw.log.Noticef("shutdown lines=%d", len(sw.lines))

	for _, line := range sw.lines {
		sw.runloop.CastAfter(10*time.Second, &cmd_line_close_broken{line})
	}

	return nil
}

type cmd_rcv_pkt struct {
	pkt *pkt_t
}

func (cmd *cmd_rcv_pkt) Exec(state interface{}) error {
	var (
		sw   = state.(*Switch)
		pkt  = cmd.pkt
		peer *Peer
	)

	if pkt.priv_hdr.Type == "line" {
		line := sw.active_lines[pkt.priv_hdr.Line]

		if line == nil {
			sw.log.Errorf("line: error: %s", errUnknownLine)
			return nil
		}

		cmd.rcv_line_pkt(line, pkt)
		return nil
	}

	if pkt.priv_hdr.Type == "open" {
		pub, err := decompose_open_pkt(sw.Key, pkt)
		if err != nil {
			sw.log.Errorf("open: error: %s", err)
			return nil
		}

		{ // get the peer
			cmd := cmd_peer_get{pub.hashname, true, nil}
			cmd.Exec(sw)
			peer = cmd.peer
		}

		had_net_paths := len(peer.net_paths()) == 0
		peer.add_net_path(pkt.netpath)
		peer.set_public_key(pub.rsa_pubkey)
		if !had_net_paths {
			peer.set_active_paths(peer.net_paths())
		}

		line := sw.lines[peer.Hashname()]
		if line == nil {
			line = &line_t{}
			line.Init(sw, peer)
			sw.lines[peer.Hashname()] = line
			sw.met_running_lines.Update(int64(len(sw.lines)))
		}

		cmd.rcv_open_pkt(line, pub, pkt.netpath)
		return nil
	}

	// drop
	return nil
}

func (cmd *cmd_rcv_pkt) rcv_line_pkt(l *line_t, opkt *pkt_t) error {
	ipkt, err := l.shr_key.dec(opkt)
	if err != nil {
		return err
	}

	l.broken_timer.Reset(line_broken_timeout)

	ipkt.peer = l.peer
	ipkt.netpath = opkt.netpath

	if ipkt.priv_hdr.C != "" && ipkt.priv_hdr.Type == "relay" {
		l.sw.relay_handler.rcv(ipkt)
		return nil
	}

	if ipkt.priv_hdr.C == "" {
		return errInvalidPkt
	}

	// send pkt to existing channel
	if channel := l.channels[ipkt.priv_hdr.C]; channel != nil {
		l.peer.add_net_path(ipkt.netpath)
		channel.log.Debugf("rcv pkt:\nhdr=%s\nprv-hdr=%+v", ipkt.hdr, ipkt.priv_hdr)
		return channel.push_rcv_pkt(ipkt)
	}

	// open new channel
	if ipkt.priv_hdr.Type == "" {
		return errInvalidPkt
	}

	reliablility := ReliableChannel
	if !ipkt.priv_hdr.Seq.IsSet() {
		reliablility = UnreliableChannel
	}

	if reliablility == ReliableChannel && ipkt.priv_hdr.Seq.Get() != 0 {
		return errInvalidPkt
	}

	options := ChannelOptions{peer: l.peer, Id: ipkt.priv_hdr.C, Type: ipkt.priv_hdr.Type, Reliablility: reliablility}
	channel, err := make_channel(l.sw, l, false, options)
	if err != nil {
		return err
	}

	if l.channels[channel.Id()] != nil {
		return errChannelAlreadyExists
	}

	l.channels[channel.Id()] = channel
	l.sw.met_channels.Inc(1)

	l.log.Debugf("channel[%s:%s](%s -> %s): opened (initiator=false)",
		short_hash(channel.Id()),
		channel.Type(),
		l.peer.Hashname().Short(),
		l.sw.hashname.Short())

	channel.log.Debugf("rcv pkt:\nhdr=%s\nprv-hdr=%+v", ipkt.hdr, ipkt.priv_hdr)
	err = channel.push_rcv_pkt(ipkt)
	if err != nil {
		return err
	}

	l.peer.add_net_path(ipkt.netpath)
	go channel.run_user_handler()

	return nil
}

func (cmd *cmd_rcv_pkt) rcv_open_pkt(l *line_t, pub *public_line_key, netpath *net_path) error {
	var (
		err            error
		local_hashname = l.sw.hashname
		reopen         bool
	)

	if l.state == line_opened {
		reopen = true
	}

	prv := l.prv_key
	if prv == nil {
		err := l.SndOpen(netpath)
		if err != nil {
			return err
		}
		prv = l.prv_key
	}
	if reopen {
		err := l.SndOpen(netpath)
		if err != nil {
			return err
		}
	}

	err = pub.verify(l.pub_key, local_hashname)
	if err != nil {
		l.log.Noticef("rcv open from=%s err=%s", netpath, err)
		return nil
	}

	shr, err := line_activate(prv, pub)
	if err != nil {
		l.log.Noticef("rcv open from=%s err=%s", netpath, err)
		return err
	}

	l.peer.set_public_key(pub.rsa_pubkey)
	l.peer.add_net_path(netpath)

	l.prv_key = prv
	l.pub_key = pub
	l.shr_key = shr

	if reopen {
		// done
		l.backlog.RescheduleAll(&l.sw.runloop)
		return nil
	}

	l.sw.active_lines[l.prv_key.id] = l
	l.sw.met_open_lines.Update(int64(len(l.sw.active_lines)))
	l.state = line_pathing
	l.log.Debugf("line pathing %s -> %s", local_hashname.Short(), l.peer.hashname.Short())

	go func() {
		if l.sw.path_handler.Negotiate(l.peer) {
			l.sw.runloop.Cast(&cmd_line_opened{l})
		} else {
			l.sw.runloop.Cast(&cmd_line_close_broken{l})
		}
	}()

	return nil
}

type cmd_line_opened struct {
	line *line_t
}

func (cmd *cmd_line_opened) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
		l  = cmd.line
	)

	l.state = line_opened

	stop_timer(l.open_timer)
	l.open_timer = nil

	l.broken_timer.Reset(line_broken_timeout)

	l.log.Debugf("line opened")

	l.backlog.RescheduleAll(&sw.runloop)
	return nil
}

type cmd_snd_pkt struct {
	channel        *Channel
	line           *line_t
	pkt            *pkt_t
	bypass_channel bool
}

func (cmd *cmd_snd_pkt) Exec(state interface{}) error {
	var (
		sw      = state.(*Switch)
		channel = cmd.channel
		line    = cmd.line
		ipkt    = cmd.pkt
		opkt    *pkt_t
		err     error
	)

	if channel != nil && !cmd.bypass_channel {
		if !channel.can_snd_pkt() {
			sw.runloop.Defer(&channel.snd_backlog)
			return nil
		}
		err = channel.will_send_packet(ipkt)
		if err != nil {
			return err
		}
	}

	ipkt.peer = line.peer

	opkt, err = line.shr_key.enc(ipkt)
	defer packet_pool_release(opkt)
	if err != nil {
		return err
	}

	sender := opkt.netpath
	if sender == nil {
		sender = line.peer.active_path()
	}
	if sender == nil {
		return ErrPeerBroken
	}
	opkt.netpath = sender

	err = sw.snd_pkt(opkt)
	if err != nil {
		return err
	}

	if channel != nil && !cmd.bypass_channel {
		channel.did_send_packet(ipkt)
	}
	if channel != nil {
		channel.log.Debugf("snd pkt:\nhdr=%s\nprv-hdr=%+v", ipkt.hdr, ipkt.priv_hdr)
	}

	// line.log.Debugf("snd pkt:\nhdr=%s\nprv-hdr=%+v", opkt.hdr, opkt.priv_hdr)
	return nil
}

type cmd_channel_open struct {
	options ChannelOptions
	channel *Channel
}

func (cmd *cmd_channel_open) Exec(state interface{}) error {
	var (
		sw      = state.(*Switch)
		line    *line_t
		channel *Channel
		err     error
	)

	if sw.terminating {
		return errNoOpenLine
	}

	line = sw.lines[cmd.options.peer.hashname]
	if line == nil {
		return cmd.open_line(sw)
	}
	if !(line.state == line_pathing && cmd.options.Type == "path") {
		if line.state != line_opened {
			sw.runloop.Defer(&line.backlog)
			return nil
		}
	}

	channel, err = make_channel(sw, line, true, cmd.options)
	if err != nil {
		return err
	}

	if line.channels[channel.Id()] != nil {
		return errChannelAlreadyExists
	}

	line.channels[channel.Id()] = channel
	sw.met_channels.Inc(1)

	line.log.Debugf("channel[%s:%s](%s -> %s): opened (initiator=true)",
		short_hash(channel.Id()),
		channel.Type(),
		sw.hashname.Short(),
		line.peer.Hashname().Short())

	cmd.channel = channel
	return nil
}

func (cmd *cmd_channel_open) open_line(sw *Switch) error {
	var (
		peer *Peer
		line *line_t
		err  error
	)

	peer = cmd.options.peer
	if peer == nil {
		// seek
		return ErrUnknownPeer
	}

	if !peer.can_open() {
		return ErrPeerBroken
	}

	line = &line_t{}
	line.Init(sw, peer)
	err = cmd.open(line, peer)
	if err != nil {
		return err
	}

	sw.lines[cmd.options.peer.hashname] = line
	sw.met_running_lines.Update(int64(len(sw.lines)))

	sw.runloop.Defer(&line.backlog)
	return nil
}

func (cmd *cmd_channel_open) open(l *line_t, peer *Peer) error {
	if len(peer.net_paths()) == 0 && len(peer.via) != 0 {
		peer.add_net_path(&net_path{Network: "relay", Address: make_relay_addr()})
	}

	if peer.pubkey == nil && len(peer.via) != 0 {
		// start with sending a peer command
		l.state = line_peering
		go l.open_with_peer()
		return nil
	} else if peer.pubkey != nil && len(peer.paths) != 0 {
		// send open
		l.state = line_opening
		return l.SndOpen(nil)
	} else {
		// unreachable peer
		// TODO seek?
		return ErrPeerBroken
	}
}

type cmd_line_close_idle struct {
	line *line_t
}

func (cmd *cmd_line_close_idle) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	cmd.line.state = line_closed

	for _, c := range cmd.line.channels {
		c.mark_as_broken()
		c.rcv_backlog.CancelAll(ErrChannelBroken)
		c.snd_backlog.CancelAll(ErrChannelBroken)
	}

	cmd.line.backlog.CancelAll(ErrChannelBroken)
	sw.met_channels.Dec(int64(len(cmd.line.channels)))

	stop_timer(cmd.line.open_timer)
	stop_timer(cmd.line.broken_timer)

	if cmd.line.prv_key != nil {
		if _, p := sw.active_lines[cmd.line.prv_key.id]; p {
			delete(sw.active_lines, cmd.line.prv_key.id)
			sw.met_open_lines.Update(int64(len(sw.active_lines)))
		}
	}
	if cmd.line.peer != nil {
		if _, p := sw.lines[cmd.line.peer.hashname]; p {
			delete(sw.lines, cmd.line.peer.hashname)
			sw.met_running_lines.Update(int64(len(sw.lines)))
		}
	}

	cmd.line.log.Noticef("line closed: peer=%s (reason=%s)",
		cmd.line.peer.String(),
		"idle")

	return nil
}

type cmd_line_close_broken struct {
	line *line_t
}

func (cmd *cmd_line_close_broken) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	cmd.line.state = line_closed

	for _, c := range cmd.line.channels {
		c.mark_as_broken()
		c.rcv_backlog.CancelAll(ErrChannelBroken)
		c.snd_backlog.CancelAll(ErrChannelBroken)
	}

	cmd.line.backlog.CancelAll(ErrChannelBroken)
	sw.met_channels.Dec(int64(len(cmd.line.channels)))

	stop_timer(cmd.line.open_timer)
	stop_timer(cmd.line.broken_timer)

	if cmd.line.prv_key != nil {
		if _, p := sw.active_lines[cmd.line.prv_key.id]; p {
			delete(sw.active_lines, cmd.line.prv_key.id)
			sw.met_open_lines.Update(int64(len(sw.active_lines)))
		}
	}
	if cmd.line.peer != nil {
		if _, p := sw.lines[cmd.line.peer.hashname]; p {
			delete(sw.lines, cmd.line.peer.hashname)
			sw.met_running_lines.Update(int64(len(sw.lines)))
		}
	}

	cmd.line.log.Noticef("line closed: peer=%s (reason=%s)",
		cmd.line.peer.String(),
		"broken")

	return nil
}

type cmd_line_close_down struct {
	line *line_t
}

func (cmd *cmd_line_close_down) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	cmd.line.state = line_closed

	for _, c := range cmd.line.channels {
		c.mark_as_broken()
		c.rcv_backlog.CancelAll(ErrChannelBroken)
		c.snd_backlog.CancelAll(ErrChannelBroken)
	}

	cmd.line.backlog.CancelAll(ErrChannelBroken)
	sw.met_channels.Dec(int64(len(cmd.line.channels)))

	stop_timer(cmd.line.open_timer)
	stop_timer(cmd.line.broken_timer)

	if cmd.line.prv_key != nil {
		if _, p := sw.active_lines[cmd.line.prv_key.id]; p {
			delete(sw.active_lines, cmd.line.prv_key.id)
			sw.met_open_lines.Update(int64(len(sw.active_lines)))
		}
	}
	if cmd.line.peer != nil {
		if _, p := sw.lines[cmd.line.peer.hashname]; p {
			delete(sw.lines, cmd.line.peer.hashname)
			sw.met_running_lines.Update(int64(len(sw.lines)))
		}
	}

	cmd.line.log.Noticef("line closed: peer=%s (reason=%s)",
		cmd.line.peer.String(),
		"peer down")

	return nil
}

type cmd_line_snd_path struct {
	line *line_t
}

func (cmd *cmd_line_snd_path) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	if sw.terminating {
		return nil
	}

	if cmd.line.state != line_opened {
		return nil
	}

	go func() {
		var (
			l = cmd.line
		)

		if l.last_sync.After(time.Now().Add(-120 * time.Second)) {
			if sw.path_handler.negotiate_netpath(l.peer, l.peer.active_path()) {
				return
			}
			// else do full negotioation
		}

		if sw.path_handler.Negotiate(l.peer) {
			l.last_sync = time.Now()
			return
		}

		sw.runloop.Cast(&cmd_line_close_broken{l})
		l.log.Noticef("path failed (breaking the line)")
	}()

	return nil
}

type cmd_get_rcv_pkt struct {
	channel *Channel
	pkt     *pkt_t
	err     error
}

func (cmd *cmd_get_rcv_pkt) Exec(state interface{}) error {
	var (
		sw      = state.(*Switch)
		channel = cmd.channel
	)

	if !channel.can_pop_rcv_pkt() {
		sw.runloop.Defer(&channel.rcv_backlog)
		return nil
	}

	pkt, err := channel.pop_rcv_pkt()
	cmd.pkt = pkt

	if err == nil && pkt == nil {
		sw.runloop.Defer(&channel.rcv_backlog)
		return nil
	}

	return err
}

type cmd_channel_set_rcv_deadline struct {
	channel  *Channel
	deadline time.Time
}

func (cmd *cmd_channel_set_rcv_deadline) Exec(state interface{}) error {
	var (
		sw       = state.(*Switch)
		channel  = cmd.channel
		deadline = cmd.deadline
		now      = time.Now()
	)

	switch {

	case deadline.IsZero():
		// unset deadline
		channel.rcv_deadline_reached = false
		if channel.rcv_deadline != nil {
			stop_timer(channel.rcv_deadline)
			channel.rcv_deadline = nil
		}

	case deadline.Before(now):
		// deadline reached (.deadline is in the past)
		channel.rcv_deadline_reached = true
		if channel.rcv_deadline != nil {
			stop_timer(channel.rcv_deadline)
			channel.rcv_deadline = nil
		}
		channel.reschedule()

	default:
		// deadline scheduled (.deadline is in the future)
		channel.rcv_deadline_reached = false
		if channel.rcv_deadline != nil {
			channel.rcv_deadline.Reset(deadline.Sub(now))
		} else {
			channel.rcv_deadline = sw.runloop.CastAfter(deadline.Sub(now), &cmd_channel_deadline_reached{channel})
		}

	}

	return nil
}

type cmd_channel_deadline_reached struct {
	channel *Channel
}

func (cmd *cmd_channel_deadline_reached) Exec(state interface{}) error {
	var (
		channel = cmd.channel
	)

	channel.rcv_deadline_reached = true
	channel.reschedule()
	return nil
}

type cmd_stats_log struct{}

func (cmd *cmd_stats_log) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	sw.log.Noticef("stats: %s", sw.Stats())
	sw.runloop.CastAfter(5*time.Second, cmd)
	return nil
}

type cmd_clean struct{}

func (cmd *cmd_clean) Exec(state interface{}) error {
	var (
		sw = state.(*Switch)
	)

	if sw.terminating {
		return nil
	}

	for _, l := range sw.lines {
		for i, c := range l.channels {
			if c.is_closed() {
				l.log.Debugf("channel[%s:%s](%s -> %s): closed",
					short_hash(c.Id()),
					c.Type(),
					sw.hashname.Short(),
					l.peer.Hashname().Short())
				delete(l.channels, i)
				sw.met_channels.Dec(1)
			}
		}
	}

	sw.runloop.CastAfter(2*time.Second, cmd)
	return nil
}

func stop_timer(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}
