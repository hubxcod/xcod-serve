package stream

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// segmenter cuts MPEG-TS into HLS segments, always starting at a keyframe and with PAT/PMT
// at the beginning, so each segment can be decoded on its own. It is not safe for concurrent
// use: the channel guards it with ch.mu.
type segmenter struct {
	target float64 // target segment duration (s)
	window int     // segments listed in the playlist

	pending  []byte // incomplete TS packet from the previous read
	pat, pmt []byte // last tables seen (one packet each)
	pmtPID   int
	keyPID   int  // PID used for cuts (video, or audio if there is none)
	keyType  byte // stream_type from the PMT (0 = audio: any PES start works)

	started  bool
	cur      []byte
	curPTS   int64
	curStart time.Time
	curDisc  bool
	nextDisc bool

	segs    []*segment
	nextSeq int
	dseq    int           // discontinuities emitted so far
	updated chan struct{} // closed (and replaced) on every new segment
}

type segment struct {
	seq  int
	dur  float64
	disc bool
	dseq int // discontinuities up to this segment (inclusive)
	data []byte
}

func newSegmenter(target float64, window int) *segmenter {
	s := &segmenter{target: target, window: window, updated: make(chan struct{})}
	// clock-based sequence: keeps growing if the channel restarts
	s.nextSeq = int(time.Now().Unix())
	s.reset()
	s.nextDisc = false
	return s
}

// reset is called on every new upstream connection.
func (s *segmenter) reset() {
	s.pending = nil
	s.pat, s.pmt = nil, nil
	s.pmtPID, s.keyPID, s.keyType = -1, -1, 0
	s.started = false
	s.cur = nil
	s.nextDisc = len(s.segs) > 0
}

func (s *segmenter) feed(data []byte) {
	buf := append(s.pending, data...)
	i := 0
	for i+188 <= len(buf) {
		if buf[i] != 0x47 { // lost sync: look for the next 0x47
			i++
			continue
		}
		s.packet(buf[i : i+188])
		i += 188
	}
	s.pending = append([]byte(nil), buf[i:]...)
}

func (s *segmenter) packet(p []byte) {
	pid := int(p[1]&0x1f)<<8 | int(p[2])
	pusi := p[1]&0x40 != 0
	afc := (p[3] >> 4) & 3
	off := 4
	if afc&2 != 0 {
		off += 1 + int(p[4])
	}
	var payload []byte
	if afc&1 != 0 && off < 188 {
		payload = p[off:]
	}

	switch {
	case pid == 0 && pusi && payload != nil:
		s.pat = append([]byte(nil), p...)
		s.parsePAT(payload)
	case pid == s.pmtPID && pusi && payload != nil:
		s.pmt = append([]byte(nil), p...)
		s.parsePMT(payload)
	}

	if pid == s.keyPID && pusi && payload != nil {
		key := s.isKeyframe(p, payload)
		pts := parsePTS(payload)
		switch {
		case !s.started && key:
			s.startSegment(pts)
		case s.started && key && s.elapsed(pts) >= s.target:
			s.finishSegment(pts)
			s.startSegment(pts)
		case s.started && s.elapsed(pts) >= s.target*3:
			// source without signalled keyframes: cut anyway
			s.finishSegment(pts)
			s.startSegment(pts)
		}
	}
	if s.started {
		s.cur = append(s.cur, p...)
	}
}

// section returns the PSI table after the pointer_field, with bounds checked.
func section(payload []byte) []byte {
	if len(payload) < 1 {
		return nil
	}
	start := 1 + int(payload[0])
	if start+3 > len(payload) {
		return nil
	}
	t := payload[start:]
	end := 3 + (int(t[1]&0x0f)<<8 | int(t[2]))
	if end > len(t) {
		return nil
	}
	return t[:end]
}

func (s *segmenter) parsePAT(payload []byte) {
	t := section(payload)
	if len(t) < 12 || t[0] != 0x00 {
		return
	}
	for j := 8; j+4 <= len(t)-4; j += 4 {
		if prog := int(t[j])<<8 | int(t[j+1]); prog != 0 {
			s.pmtPID = int(t[j+2]&0x1f)<<8 | int(t[j+3])
			return
		}
	}
}

func (s *segmenter) parsePMT(payload []byte) {
	t := section(payload)
	if len(t) < 16 || t[0] != 0x02 || s.keyPID >= 0 {
		return
	}
	firstPID := -1
	j := 12 + (int(t[10]&0x0f)<<8 | int(t[11]))
	for j+5 <= len(t)-4 {
		st := t[j]
		epid := int(t[j+1]&0x1f)<<8 | int(t[j+2])
		if firstPID < 0 {
			firstPID = epid
		}
		if st == 0x1b || st == 0x24 || st == 0x02 { // H.264, HEVC, MPEG-2
			s.keyPID, s.keyType = epid, st
			return
		}
		j += 5 + (int(t[j+3]&0x0f)<<8 | int(t[j+4]))
	}
	if firstPID >= 0 { // audio only (radio): cut at any PES
		s.keyPID, s.keyType = firstPID, 0
	}
}

func (s *segmenter) isKeyframe(p, payload []byte) bool {
	if s.keyType == 0 {
		return true
	}
	// random_access_indicator in the adaptation field
	if (p[3]>>4)&2 != 0 && p[4] > 0 && p[5]&0x40 != 0 {
		return true
	}
	// otherwise look for an IDR/SPS NAL at the start of the PES
	if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
		return false
	}
	es := payload[min(9+int(payload[8]), len(payload)):]
	for k := 0; k+3 < len(es); k++ {
		if es[k] != 0 || es[k+1] != 0 || es[k+2] != 1 {
			continue
		}
		b := es[k+3]
		switch s.keyType {
		case 0x1b:
			if t := b & 0x1f; t == 5 || t == 7 {
				return true
			}
		case 0x24:
			if t := (b >> 1) & 0x3f; (t >= 16 && t <= 21) || t == 32 || t == 33 {
				return true
			}
		case 0x02:
			if b == 0xb3 {
				return true
			}
		}
	}
	return false
}

// parsePTS extracts the PTS (90 kHz) from the PES header, or -1.
func parsePTS(pes []byte) int64 {
	if len(pes) < 14 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 || pes[7]&0x80 == 0 {
		return -1
	}
	return int64(pes[9]>>1&7)<<30 | int64(pes[10])<<22 | int64(pes[11]>>1)<<15 |
		int64(pes[12])<<7 | int64(pes[13]>>1)
}

// elapsed is the current segment duration: from the PTS, or from the clock if there is none.
func (s *segmenter) elapsed(pts int64) float64 {
	if pts >= 0 && s.curPTS >= 0 {
		d := pts - s.curPTS
		if d < 0 {
			d += 1 << 33 // 33-bit counter wrap-around
		}
		if d < 90000*60 {
			return float64(d) / 90000
		}
	}
	return time.Since(s.curStart).Seconds()
}

func (s *segmenter) startSegment(pts int64) {
	s.started = true
	s.cur = make([]byte, 0, 1<<20)
	s.cur = append(s.cur, s.pat...)
	s.cur = append(s.cur, s.pmt...)
	s.curPTS = pts
	s.curStart = time.Now()
	s.curDisc = s.nextDisc
	s.nextDisc = false
}

func (s *segmenter) finishSegment(pts int64) {
	if s.curDisc {
		s.dseq++
	}
	s.segs = append(s.segs, &segment{seq: s.nextSeq, dur: s.elapsed(pts), disc: s.curDisc, dseq: s.dseq, data: s.cur})
	s.nextSeq++
	// keep a few beyond the window for clients still downloading
	if keep := s.window + 3; len(s.segs) > keep {
		s.segs = append(s.segs[:0:0], s.segs[len(s.segs)-keep:]...)
	}
	close(s.updated)
	s.updated = make(chan struct{})
}

// playlist returns the live playlist, or "" if there are not enough segments yet. prefix is
// the segments' relative path (e.g. "846/").
func (s *segmenter) playlist(prefix string) string {
	if len(s.segs) < 2 {
		return ""
	}
	list := s.segs[max(0, len(s.segs)-s.window):]
	maxDur := 0.0
	for _, sg := range list {
		maxDur = math.Max(maxDur, sg.dur)
	}
	first := list[0]
	dseq := first.dseq
	if first.disc {
		dseq--
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxDur)))
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", first.seq)
	fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", dseq)
	for _, sg := range list {
		if sg.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s%d.ts\n", sg.dur, prefix, sg.seq)
	}
	return b.String()
}

func (s *segmenter) get(seq int) []byte {
	for _, sg := range s.segs {
		if sg.seq == seq {
			return sg.data
		}
	}
	return nil
}
