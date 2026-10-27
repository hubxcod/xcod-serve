package config

// Quality is a transcoding profile. It uses CRF with a bitrate ceiling (VBV): simple scenes
// use much less than the ceiling, complex scenes never go above it.
type Quality struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Height    int    `json:"height"`     // 0 = original (no transcoding)
	VideoKbps int    `json:"video_kbps"` // video ceiling
	AudioKbps int    `json:"audio_kbps"`
	CRF       int    `json:"crf"`
}

var Qualities = []Quality{
	{Name: "original", Label: "Original (no transcoding)"},
	{Name: "1080p", Label: "1080p · up to 4.5 Mbps", Height: 1080, VideoKbps: 4500, AudioKbps: 128, CRF: 23},
	{Name: "720p", Label: "720p · up to 2.5 Mbps", Height: 720, VideoKbps: 2500, AudioKbps: 128, CRF: 23},
	{Name: "720p-eco", Label: "720p economy · up to 1.5 Mbps", Height: 720, VideoKbps: 1500, AudioKbps: 96, CRF: 25},
	{Name: "480p", Label: "480p · up to 1 Mbps", Height: 480, VideoKbps: 1000, AudioKbps: 96, CRF: 24},
	{Name: "360p", Label: "360p · up to 0.6 Mbps", Height: 360, VideoKbps: 600, AudioKbps: 64, CRF: 25},
}

var EncoderPresets = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium"}

func FindQuality(name string) (Quality, bool) {
	if name == "" {
		name = "original"
	}
	for _, q := range Qualities {
		if q.Name == name {
			return q, true
		}
	}
	return Quality{}, false
}
