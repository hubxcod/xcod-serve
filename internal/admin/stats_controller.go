package admin

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
)

func (s *Server) statsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "stats", "Statistics", nil)
}

func (s *Server) statsJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.meter.Snapshot())
}

// statsCSV exports the daily totals per channel (opens in spreadsheet tools).
func (s *Server) statsCSV(w http.ResponseWriter, r *http.Request) {
	snap := s.meter.Snapshot()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="xcod-stats.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"date", "channel", "downloaded_bytes", "sent_bytes", "peak_viewers", "viewer_minutes"})
	days := make([]string, 0, len(snap.Days))
	for d := range snap.Days {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		for _, id := range sortedKeys(snap.Days[d]) {
			v := snap.Days[d][id]
			cw.Write([]string{d, id, strconv.FormatInt(v.Down, 10), strconv.FormatInt(v.Up, 10),
				strconv.Itoa(v.PeakViewers), strconv.FormatInt(v.ViewerMinutes, 10)})
		}
	}
	cw.Flush()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
