// Package stats measures bandwidth and resource usage: bytes received from the provider and
// sent to viewers, CPU (xcod + ffmpeg) and memory. It keeps 24h of per-minute history and
// daily totals per channel in stats.json.
package stats

import (
	"encoding/json"
	"log"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Counters adds up a channel's bytes. They are never removed, so the totals lose nothing
// when a channel is changed or deleted.
type Counters struct{ Src, Out atomic.Int64 }

// ChannelLoad is a channel's state at one instant (from the hub).
type ChannelLoad struct {
	Viewers int
	Running bool
}

// Source returns the current load of each channel.
type Source func() map[string]ChannelLoad

// Sample is one minute of history.
type Sample struct {
	T         int64   `json:"t"`    // unix time at the end of the minute
	Down      int64   `json:"down"` // bytes received from the provider
	Up        int64   `json:"up"`   // bytes sent to viewers
	Viewers   int     `json:"viewers"`
	Upstreams int     `json:"upstreams"`
	CPU       float64 `json:"cpu"` // % of one core (xcod + ffmpeg)
	MemMB     float64 `json:"mem_mb"`
}

type Day struct {
	Down          int64 `json:"down"`
	Up            int64 `json:"up"`
	PeakViewers   int   `json:"peak_viewers"`
	ViewerMinutes int64 `json:"viewer_minutes"`
}

type file struct {
	Series []Sample                   `json:"series"` // last 24h
	Days   map[string]map[string]*Day `json:"days"`   // "2006-01-02" -> channel -> totals
}

const (
	seriesMax = 24 * 60
	daysMax   = 90
)

type Meter struct {
	path    string
	started time.Time

	mu   sync.Mutex
	byID map[string]*Counters
	last map[string][2]int64 // counters at the last sample
	data file

	cpuNow, cpuMin   float64 // % over the last 5s interval / last minute
	cpuPrev, cpuMark time.Duration
	prevAt, markAt   time.Time
}

// New loads the history from path; a missing or invalid file starts from scratch.
func New(path string) *Meter {
	m := &Meter{path: path, started: time.Now(), byID: map[string]*Counters{}, last: map[string][2]int64{},
		data: file{Days: map[string]map[string]*Day{}}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("stats: invalid %s, starting from scratch: %v", path, err)
		return m
	}
	if f.Days == nil {
		f.Days = map[string]map[string]*Day{}
	}
	m.data = f
	return m
}

func (m *Meter) Counters(id string) *Counters {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.byID[id]
	if c == nil {
		c = &Counters{}
		m.byID[id] = c
	}
	return c
}

// Run samples the CPU every 5s and closes one minute of history every 60s.
func (m *Meter) Run(src Source, stop <-chan struct{}) {
	now := time.Now()
	m.mu.Lock()
	m.cpuPrev, m.cpuMark = cpuTime(), cpuTime()
	m.prevAt, m.markAt = now, now
	m.mu.Unlock()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			total := cpuTime()
			m.mu.Lock()
			m.cpuNow = pct(total-m.cpuPrev, now.Sub(m.prevAt))
			m.cpuPrev, m.prevAt = total, now
			minute := now.Sub(m.markAt) >= time.Minute
			if minute {
				m.cpuMin = pct(total-m.cpuMark, now.Sub(m.markAt))
				m.cpuMark, m.markAt = total, now
			}
			m.mu.Unlock()
			if minute {
				m.Record(src, now)
			}
		}
	}
}

func pct(cpu, wall time.Duration) float64 {
	if wall <= 0 {
		return 0
	}
	return float64(cpu) / float64(wall) * 100
}

func memMB() float64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.Sys) / (1 << 20)
}

// Record closes the period up to now: history, daily totals and saving to disk. It is also
// called on shutdown, so the last minute is not lost.
func (m *Meter) Record(src Source, now time.Time) {
	load := src()
	mem := memMB()

	m.mu.Lock()
	day := now.Format("2006-01-02")
	days := m.data.Days[day]
	if days == nil {
		days = map[string]*Day{}
		m.data.Days[day] = days
	}
	s := Sample{T: now.Unix(), CPU: m.cpuMin, MemMB: mem}
	for id, c := range m.byID {
		src, out := c.Src.Load(), c.Out.Load()
		l := m.last[id]
		m.last[id] = [2]int64{src, out}
		down, up := src-l[0], out-l[1]
		viewers := load[id].Viewers
		s.Down += down
		s.Up += up
		if down == 0 && up == 0 && viewers == 0 {
			continue
		}
		d := days[id]
		if d == nil {
			d = &Day{}
			days[id] = d
		}
		d.Down += down
		d.Up += up
		d.PeakViewers = max(d.PeakViewers, viewers)
		d.ViewerMinutes += int64(viewers)
	}
	for _, l := range load {
		s.Viewers += l.Viewers
		if l.Running {
			s.Upstreams++
		}
	}
	m.data.Series = append(m.data.Series, s)
	if n := len(m.data.Series); n > seriesMax {
		m.data.Series = append(m.data.Series[:0:0], m.data.Series[n-seriesMax:]...)
	}
	if len(m.data.Days) > daysMax {
		keys := make([]string, 0, len(m.data.Days))
		for k := range m.data.Days {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-daysMax] {
			delete(m.data.Days, k)
		}
	}
	raw, err := json.Marshal(m.data)
	m.mu.Unlock()

	if err == nil {
		tmp := m.path + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, m.path)
		}
	}
	if err != nil {
		log.Printf("stats: could not save %s: %v", m.path, err)
	}
}

// Snapshot is what the panel receives.
type Snapshot struct {
	Series    []Sample                  `json:"series"`
	Days      map[string]map[string]Day `json:"days"`
	Since     int64                     `json:"since"`
	TotalDown int64                     `json:"total_down"` // since xcod started
	TotalUp   int64                     `json:"total_up"`
	CPU       float64                   `json:"cpu"`
	Cores     int                       `json:"cores"`
	MemMB     float64                   `json:"mem_mb"`
	FFmpeg    int                       `json:"ffmpeg"`
}

// Usage is the current resource usage, without history (lightweight, for the live dashboard).
type Usage struct {
	CPU    float64 `json:"cpu"`
	Cores  int     `json:"cores"`
	MemMB  float64 `json:"mem_mb"`
	FFmpeg int     `json:"ffmpeg"`
}

func (m *Meter) Usage() Usage {
	m.mu.Lock()
	cpu := m.cpuNow
	m.mu.Unlock()
	return Usage{CPU: cpu, Cores: runtime.NumCPU(), MemMB: memMB(), FFmpeg: FFmpegCount()}
}

func (m *Meter) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Series: append([]Sample(nil), m.data.Series...), Days: map[string]map[string]Day{},
		Since: m.started.Unix(), CPU: m.cpuNow, Cores: runtime.NumCPU(), MemMB: memMB(), FFmpeg: FFmpegCount()}
	for day, chans := range m.data.Days {
		cp := make(map[string]Day, len(chans))
		for id, d := range chans {
			cp[id] = *d
		}
		s.Days[day] = cp
	}
	for _, c := range m.byID {
		s.TotalDown += c.Src.Load()
		s.TotalUp += c.Out.Load()
	}
	return s
}

// ---------- CPU of xcod + ffmpeg processes ----------

var ffmpegs = struct {
	sync.Mutex
	live map[int]struct{}
	dead time.Duration // CPU of ffmpeg processes that already ended
}{live: map[int]struct{}{}}

func FFmpegStarted(pid int) {
	ffmpegs.Lock()
	ffmpegs.live[pid] = struct{}{}
	ffmpegs.Unlock()
}

func FFmpegEnded(pid int, cpu time.Duration) {
	ffmpegs.Lock()
	delete(ffmpegs.live, pid)
	ffmpegs.dead += cpu
	ffmpegs.Unlock()
}

func FFmpegCount() int {
	ffmpegs.Lock()
	defer ffmpegs.Unlock()
	return len(ffmpegs.live)
}

// cpuTime is the accumulated CPU time of xcod and every ffmpeg it started. It only grows, so
// the difference between two readings is the usage in that period.
func cpuTime() time.Duration {
	total, _ := procCPU(os.Getpid())
	ffmpegs.Lock()
	defer ffmpegs.Unlock()
	total += ffmpegs.dead
	for pid := range ffmpegs.live {
		if t, ok := procCPU(pid); ok {
			total += t
		}
	}
	return total
}
