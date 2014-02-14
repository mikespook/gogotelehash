package kademlia

import (
	"github.com/telehash/gogotelehash"
	"github.com/telehash/gogotelehash/runloop"
	"sync"
)

type DHT struct {
	DisableSeed bool
	Seeds       []*telehash.Identity
	sw          *telehash.Switch
	table       seek_table
	links       map[telehash.Hashname]*link_t
	runloop     runloop.RunLoop
}

func (d *DHT) Start(sw *telehash.Switch, wg *sync.WaitGroup) error {
	d.runloop.State = d
	d.sw = sw
	d.links = make(map[telehash.Hashname]*link_t)
	d.table.Init(sw.LocalHashname())
	telehash.InternalMux(sw).HandleFunc("seek", d.serve_seek)
	telehash.InternalMux(sw).HandleFunc("link", d.serve_link)

	d.runloop.Run()

	wg.Add(1)
	go func() {
		defer wg.Done()

		for _, seed := range d.Seeds {
			peer := seed.ToPeer(sw)
			if peer != nil {
				d.open_link(peer)
			}
		}
	}()

	return nil
}

func (d *DHT) Stop() error {
	d.runloop.StopAndWait()
	return nil
}

func (d *DHT) GetPeer(hashname telehash.Hashname) *telehash.Peer {
	link := d.get_link(hashname)
	if link != nil {
		return link.peer
	}
	return nil
}

func (d *DHT) get_link(hashname telehash.Hashname) *link_t {
	cmd := cmd_link_get{hashname, nil}
	d.runloop.Call(&cmd)
	return cmd.link
}

func (d *DHT) closest_links(target telehash.Hashname, num int) []*link_t {
	cmd := cmd_link_closest{target, num, nil}
	d.runloop.Call(&cmd)
	return cmd.links
}

func (d *DHT) OnNewPeer(peer *telehash.Peer) {
	d.runloop.Cast(&evt_peer_new{peer})
}

func (d *DHT) Seek(target telehash.Hashname) (*telehash.Peer, error) {
	// try local first
	if link := d.get_link(target); link != nil {
		return link.peer, nil
	}

	var (
		links   = d.closest_links(target, 5)
		in      = make(chan *telehash.Peer, len(links))
		out     = make(chan *telehash.Peer)
		skip    = map[telehash.Hashname]bool{}
		pending int
	)

	defer close(in)
	defer close(out)

	if len(links) == 0 {
		return nil, telehash.ErrPeerNotFound
	}

	// start some workers
	for i := 0; i < 5; i++ {
		go d.do_seek(target, in, out)
	}

	// enqueue with closest known links
	for _, link := range links {
		skip[link.peer.Hashname()] = true
		pending++
		in <- link.peer
	}

	// handle results
	for peer := range out {
		// detect seek exhaustion
		if peer == nil {
			pending--
			if pending == 0 {
				break
			}
		}

		via := peer.Hashname()
		if via == target {
			return peer, nil // found peer
		} else if !skip[via] {
			// try to continue seeking
			skip[via] = true
			pending++
			in <- peer
		}
	}

	return nil, telehash.ErrPeerNotFound
}

func (d *DHT) SeekClosest(target telehash.Hashname, n int) ([]*telehash.Peer, error) {
	if n < 1 {
		return nil, nil
	}

	// get n closest known peers
	links := d.closest_links(target, n)

	if len(links) == 0 {
		return nil, nil
	}

	var (
		in         = make(chan *telehash.Peer, len(links))
		out        = make(chan *telehash.Peer)
		skip       = map[telehash.Hashname]bool{}
		candidates []*telehash.Peer
		pending    int
	)

	defer close(in)
	defer close(out)

	// start some workers
	for i := 0; i < 5; i++ {
		go d.do_seek(target, in, out)
	}

	// enqueue with closest known links
	for _, link := range links {
		skip[link.peer.Hashname()] = true
		pending++
		in <- link.peer
	}

	// handle results
	for peer := range out {
		// detect seek exhaustion
		if peer == nil {
			pending--
			if pending == 0 {
				break
			}
		}

		candidates = append(candidates, peer)

		via := peer.Hashname()
		if !skip[via] {
			// try to continue seeking
			skip[via] = true
			pending++
			in <- peer
		}
	}

	kad_sort_peers(target, candidates)

	if len(candidates) > n {
		candidates = candidates[:n]
	}

	return candidates, nil
}
