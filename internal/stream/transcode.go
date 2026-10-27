package stream

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"xcod/internal/config"
	"xcod/internal/stats"
)

// FFmpegBinary resolves the executable: configuration, PATH or common locations.
func FFmpegBinary(configured string) string {
	if configured != "" {
		return configured
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ffmpegArgs builds the command line: MPEG-TS in (stdin) and out (stdout).
func ffmpegArgs(q config.Quality, cc config.Channel, c *config.Config) []string {
	vkbps := q.VideoKbps
	if cc.VideoKbps > 0 {
		vkbps = cc.VideoKbps
	}
	// deinterlace interlaced frames only; never upscale
	vf := []string{"yadif=deint=interlaced"}
	if cc.FPS > 0 {
		vf = append(vf, "fps="+strconv.Itoa(cc.FPS))
	}
	vf = append(vf, fmt.Sprintf(`scale=-2:min(ih\,%d)`, q.Height))
	// keyframes at the HLS segment pace (or every 2s if the segment length is even): regular
	// segments and fast joins
	gop := 2
	if c.HLSSegment%2 != 0 {
		gop = c.HLSSegment
	}

	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-fflags", "+genpts+discardcorrupt", "-i", "pipe:0",
		"-map", "0:v:0?", "-map", "0:a:0?",
		"-vf", strings.Join(vf, ","),
		"-c:v", "libx264", "-preset", c.EncoderPreset, "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-crf", strconv.Itoa(q.CRF),
		"-maxrate", fmt.Sprintf("%dk", vkbps), "-bufsize", fmt.Sprintf("%dk", vkbps*2),
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", gop),
		"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", q.AudioKbps), "-ac", "2", "-ar", "48000",
		"-f", "mpegts", "-mpegts_flags", "+resend_headers", "-muxdelay", "0", "pipe:1",
	}
}

// transcoder is an ffmpeg process tied to one upstream session.
type transcoder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   io.ReadCloser
}

func startTranscoder(ctx context.Context, id string, q config.Quality, cc config.Channel, c *config.Config) (*transcoder, error) {
	bin := FFmpegBinary(c.FFmpegPath)
	if bin == "" {
		return nil, fmt.Errorf("ffmpeg not found (install it or set its path in the settings)")
	}
	cmd := exec.CommandContext(ctx, bin, ffmpegArgs(q, cc, c)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %v", err)
	}
	stats.FFmpegStarted(cmd.Process.Pid)
	go func() {
		sc := bufio.NewScanner(stderr)
		for n := 0; sc.Scan(); n++ {
			if n < 50 { // avoid flooding the log with a corrupted source
				log.Printf("[%s] ffmpeg: %s", id, sc.Text())
			}
		}
	}()
	log.Printf("[%s] transcoding: %s (preset %s)", id, q.Name, c.EncoderPreset)
	return &transcoder{cmd: cmd, stdin: stdin, out: out}, nil
}

func (t *transcoder) close() {
	t.stdin.Close()
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
	t.cmd.Wait()
	var cpu time.Duration
	if ps := t.cmd.ProcessState; ps != nil {
		cpu = ps.UserTime() + ps.SystemTime()
	}
	stats.FFmpegEnded(t.cmd.Process.Pid, cpu)
}
