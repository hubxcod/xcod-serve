package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"xcod/internal/config"
	"xcod/internal/stats"
)

var httpClient = &http.Client{} // no global timeout: the stream never ends

// kickBan is how long a kicked viewer cannot come back; without it the player reconnects
// right away and the kick has no effect.
const kickBan = time.Minute

// ---------- viewers ----------

// client is a TS viewer (continuous connection).
type client struct {
	ch     chan []byte
	closed chan struct{}
	once   sync.Once
	addr   string
	ip     string
	key    string
	rep    string // repeater name ("" = viewer)
}

func (c *client) close() { c.once.Do(func() { close(c.closed) }) }

// hlsViewer is an HLS viewer: there is no connection, only periodic requests.
type hlsViewer struct {
	ip, key string
	last    time.Time
}

// ---------- channel ----------

type channel struct {
	id string
	h  *Hub

	mu         sync.Mutex
	conf       config.Channel
	cancelPull context.CancelFunc // ends the current upstream session
	cnt        *stats.Counters    // bandwidth: received from the source and sent to viewers
	clients    map[*client]struct{}
	hlsViewers map[string]*hlsViewer // key|ip|user-agent
	banned     map[string]time.Time  // ip kicked by the admin -> banned until
	running    bool
	backlog    []byte
	seg        *segmenter
	lastSeen   time.Time // last time there was a viewer
	bytesIn    int64
	started    time.Time
	stop       chan struct{} // closed to force the upstream to stop
	done       chan struct{} // closed when run() returns
}

func newChannel(h *Hub, id string, cc config.Channel) *channel {
	return &channel{id: id, h: h, conf: cc, cnt: h.meter.Counters(id), clients: map[*client]struct{}{},
		hlsViewers: map[string]*hlsViewer{}, banned: map[string]time.Time{}}
}

func (ch *channel) getConf() config.Channel {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.conf
}

// isBanned reports whether the ip was kicked by the admin less than kickBan ago.
func (ch *channel) isBanned(ip string) bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	now := time.Now()
	for k, until := range ch.banned {
		if now.After(until) {
			delete(ch.banned, k)
		}
	}
	_, ok := ch.banned[ip]
	return ok
}

// reconfigure changes quality/fps/bitrate without dropping viewers: the upstream session
// restarts and HLS marks a discontinuity.
func (ch *channel) reconfigure(cc config.Channel) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.conf = cc
	if ch.running && ch.cancelPull != nil {
		ch.cancelPull()
	}
	log.Printf("[%s] configuration changed (quality %s)", ch.id, cc.Quality)
}

// ensureRunningLocked starts the upstream if it is stopped. Requires ch.mu.
func (ch *channel) ensureRunningLocked() {
	if ch.running {
		return
	}
	c := ch.h.cfg()
	ch.running = true
	ch.started = time.Now()
	ch.stop = make(chan struct{})
	ch.done = make(chan struct{})
	ch.seg = newSegmenter(float64(c.HLSSegment), c.HLSWindow)
	go ch.run()
}

func (ch *channel) addClient(c *client) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.clients[c] = struct{}{}
	ch.lastSeen = time.Now()
	if b := alignTS(ch.backlogTail()); len(b) > 0 {
		c.ch <- append([]byte(nil), b...)
	}
	ch.ensureRunningLocked()
	if c.rep != "" {
		log.Printf("[%s] +repeater %s (%s)", ch.id, c.rep, c.ip)
		return
	}
	log.Printf("[%s] +ts %s (total %d)", ch.id, c.ip, len(ch.clients))
}

func (ch *channel) removeClient(c *client) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if _, ok := ch.clients[c]; !ok {
		return
	}
	delete(ch.clients, c)
	ch.lastSeen = time.Now()
	log.Printf("[%s] -ts %s (total %d)", ch.id, c.ip, len(ch.clients))
}

// hlsTTL is how long an HLS viewer counts as active without requesting anything.
func (ch *channel) hlsTTL() time.Duration {
	return max(time.Duration(ch.h.cfg().HLSSegment)*3*time.Second, 15*time.Second)
}

// pruneHLSLocked removes inactive HLS viewers and returns how many are left.
func (ch *channel) pruneHLSLocked() int {
	ttl := ch.hlsTTL()
	for k, v := range ch.hlsViewers {
		if time.Since(v.last) > ttl {
			delete(ch.hlsViewers, k)
			log.Printf("[%s] -hls %s (total %d)", ch.id, v.ip, len(ch.hlsViewers))
		}
	}
	return len(ch.hlsViewers)
}

// touchHLS refreshes an HLS viewer that is already known.
func (ch *channel) touchHLS(id string) bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.pruneHLSLocked()
	v, ok := ch.hlsViewers[id]
	if !ok {
		return false
	}
	v.last = time.Now()
	ch.lastSeen = v.last
	return true
}

func (ch *channel) addHLS(v viewer) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.hlsViewers[v.hlsID()] = &hlsViewer{ip: v.ip, key: v.key, last: time.Now()}
	ch.lastSeen = time.Now()
	ch.ensureRunningLocked()
	log.Printf("[%s] +hls %s (total %d)", ch.id, v.ip, len(ch.hlsViewers))
}

// screens counts the screens open with the key on this channel.
func (ch *channel) screens(key string) int {
	n := 0
	for k, c := range ch.screensByKey() {
		if k == key {
			n += c
		}
	}
	return n
}

func (ch *channel) screensByKey() map[string]int {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.pruneHLSLocked()
	out := map[string]int{}
	for c := range ch.clients {
		if c.key != "" && c.rep == "" {
			out[c.key]++
		}
	}
	for _, v := range ch.hlsViewers {
		if v.key != "" {
			out[v.key]++
		}
	}
	return out
}

// backlogTail returns only the part of the backlog within the configured size.
func (ch *channel) backlogTail() []byte {
	limit := ch.h.cfg().BacklogKB * 1024
	if len(ch.backlog) > limit {
		return ch.backlog[len(ch.backlog)-limit:]
	}
	return ch.backlog
}

func (ch *channel) broadcast(data []byte) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.bytesIn += int64(len(data))

	// grow up to twice the limit before trimming: avoids copying the whole buffer on every
	// read
	limit := ch.h.cfg().BacklogKB * 1024
	ch.backlog = append(ch.backlog, data...)
	if len(ch.backlog) > 2*limit {
		ch.backlog = append(ch.backlog[:0:0], ch.backlog[len(ch.backlog)-limit:]...)
	}
	if ch.seg != nil {
		ch.seg.feed(data)
	}

	for c := range ch.clients {
		select {
		case c.ch <- data:
		default: // slow client: drop it instead of delaying everyone
			log.Printf("[%s] slow client %s dropped", ch.id, c.ip)
			delete(ch.clients, c)
			c.close()
		}
	}
	if len(ch.clients) > 0 {
		ch.lastSeen = time.Now()
	}
}

// state reports whether the upstream is active and how many viewers (TS + HLS) there are.
func (ch *channel) state() (running bool, viewers int) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.running, len(ch.clients) + ch.pruneHLSLocked()
}

// kick disconnects every viewer of the channel. With ban, they are banned for kickBan
// (otherwise the player reconnects immediately).
func (ch *channel) kick(ban bool) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	until := time.Now().Add(kickBan)
	for c := range ch.clients {
		if c.rep != "" && ban { // the repeater stays: it kicks its own viewers itself
			continue
		}
		if ban {
			ch.banned[c.ip] = until
		}
		delete(ch.clients, c)
		c.close()
	}
	for _, v := range ch.hlsViewers {
		if ban {
			ch.banned[v.ip] = until
		}
	}
	clear(ch.hlsViewers)
	log.Printf("[%s] viewers kicked by the admin", ch.id)
}

// dropKey disconnects the viewers using the key (without banning the IP).
func (ch *channel) dropKey(key string) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	n := 0
	for c := range ch.clients {
		if c.key == key {
			delete(ch.clients, c)
			c.close()
			n++
		}
	}
	for id, v := range ch.hlsViewers {
		if v.key == key {
			delete(ch.hlsViewers, id)
			n++
		}
	}
	if n > 0 {
		log.Printf("[%s] %d viewer(s) disconnected by a key change", ch.id, n)
	}
}

// hasRepeaters reports whether any repeater is pulling the channel.
func (ch *channel) hasRepeaters() bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	for c := range ch.clients {
		if c.rep != "" {
			return true
		}
	}
	return false
}

// shutdown drops viewers and the upstream (channel removed or URL changed).
func (ch *channel) shutdown() {
	ch.kick(false)
	ch.forceStop(5 * time.Second)
}

// forceStop drops the upstream immediately and waits for it to close.
func (ch *channel) forceStop(wait time.Duration) {
	ch.mu.Lock()
	if !ch.running {
		ch.mu.Unlock()
		return
	}
	select {
	case <-ch.stop:
	default:
		close(ch.stop)
	}
	done := ch.done
	ch.mu.Unlock()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// idleLocked reports whether the channel has had no viewers for longer than idle_timeout.
func (ch *channel) idleLocked() bool {
	return len(ch.clients) == 0 && ch.pruneHLSLocked() == 0 && time.Since(ch.lastSeen) > ch.h.cfg().Idle()
}

// shouldStop returns true (and marks the channel stopped) if it went idle or was forced.
func (ch *channel) shouldStop() bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	forced := false
	select {
	case <-ch.stop:
		forced = true
	default:
	}
	if forced || ch.idleLocked() {
		ch.running = false
		ch.backlog = nil
		ch.seg = nil
		return true
	}
	return false
}

func (ch *channel) run() {
	log.Printf("[%s] upstream started", ch.id)
	defer close(ch.done)
	backoff := time.Second
	for {
		if ch.shouldStop() {
			log.Printf("[%s] upstream stopped", ch.id)
			return
		}
		n, err := ch.pull()
		if n > 0 {
			backoff = time.Second
		}
		if err != nil {
			log.Printf("[%s] upstream: %v (reconnecting in %s)", ch.id, err, backoff)
		}
		select {
		case <-time.After(backoff):
		case <-ch.stop:
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// pull runs one upstream session until error, end of stream or idleness. Returns the bytes
// read.
func (ch *channel) pull() (int64, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ch.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	ch.mu.Lock()
	if ch.seg != nil {
		ch.seg.reset() // new session: the next HLS segment marks a discontinuity
	}
	cc := ch.conf
	ch.cancelPull = cancel
	ch.mu.Unlock()

	c := ch.h.cfg()
	q, _ := config.FindQuality(cc.Quality)
	req, err := http.NewRequestWithContext(ctx, "GET", cc.URL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	if c.IsRepeater() && strings.HasPrefix(cc.URL, c.Origin+"/") {
		req.Header.Set(RepeaterHeader, RepeaterPullAuth(c.OriginToken))
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "keep-alive")

	resp, err := httpClient.Do(req) // follows the 302 -> URL with a fresh token
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %s", resp.Status)
	}

	// watchdog: cancel if no data arrives for stall_timeout
	stall := time.AfterFunc(c.Stall(), cancel)
	defer stall.Stop()

	// sink receives the source bytes: straight to the viewers, or to ffmpeg
	sink := func(b []byte) error {
		ch.broadcast(append([]byte(nil), b...))
		return nil
	}
	var ffDone chan struct{}
	if q.Height > 0 {
		tc, err := startTranscoder(ctx, ch.id, q, cc, c)
		if err != nil {
			return 0, err
		}
		ffDone = make(chan struct{})
		// stop ffmpeg and wait for its output to be drained, so nothing from the old session
		// reaches viewers after the new one starts
		defer func() {
			tc.close()
			<-ffDone
		}()
		// ffmpeg's output has a watchdog too (with extra time for startup)
		outStall := time.AfterFunc(c.Stall()*2, cancel)
		defer outStall.Stop()
		go func() {
			defer close(ffDone)
			defer cancel()
			buf := make([]byte, 188*348)
			for {
				n, err := tc.out.Read(buf)
				if n > 0 {
					outStall.Reset(c.Stall())
					ch.broadcast(append([]byte(nil), buf[:n]...))
				}
				if err != nil {
					return
				}
			}
		}()
		sink = func(b []byte) error {
			_, err := tc.stdin.Write(b)
			return err
		}
	}

	// interrupted explains a cancellation: stop, quality change or ffmpeg
	interrupted := func() (bool, error) {
		select {
		case <-ch.stop:
			return true, nil
		default:
		}
		if ch.getConf() != cc {
			return true, nil
		}
		if ffDone != nil {
			select {
			case <-ffDone:
				return true, errors.New("ffmpeg exited (see the messages above)")
			default:
			}
		}
		return false, nil
	}

	var total int64
	buf := make([]byte, 188*348) // ~64KB, a multiple of the TS packet size
	idleCheck := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(c.Stall())
			total += int64(n)
			ch.cnt.Src.Add(int64(n))
			if werr := sink(buf[:n]); werr != nil {
				if ok, ierr := interrupted(); ok {
					return total, ierr
				}
				return total, fmt.Errorf("ffmpeg: %v", werr)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, errors.New("upstream closed the connection")
			}
			if ctx.Err() != nil {
				if ok, ierr := interrupted(); ok {
					return total, ierr
				}
				return total, errors.New("upstream stalled (no data)")
			}
			return total, err
		}
		if time.Since(idleCheck) > time.Second {
			idleCheck = time.Now()
			ch.mu.Lock()
			idle := ch.idleLocked()
			ch.mu.Unlock()
			if idle {
				return total, nil
			}
		}
	}
}

// waitPlaylist waits until HLS has enough segments and returns the playlist.
func (ch *channel) waitPlaylist(timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	for {
		ch.mu.Lock()
		if ch.seg == nil {
			ch.mu.Unlock()
			return "", errors.New("channel stopped")
		}
		if pl := ch.seg.playlist("/hls/" + ch.id + "/"); pl != "" {
			ch.mu.Unlock()
			return pl, nil
		}
		upd := ch.seg.updated
		ch.mu.Unlock()
		select {
		case <-upd:
		case <-time.After(time.Second): // the segmenter may have been replaced
		case <-deadline:
			return "", errors.New("channel starting, try again")
		}
	}
}

// segment returns the bytes of an HLS segment still in memory.
func (ch *channel) segment(seq int) []byte {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.seg == nil {
		return nil
	}
	return ch.seg.get(seq)
}

// alignTS returns b from the first reliable TS sync byte (0x47 every 188 bytes).
func alignTS(b []byte) []byte {
	for i := 0; i+188*2 < len(b); i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 && b[i+376] == 0x47 {
			return b[i:]
		}
	}
	return nil
}
