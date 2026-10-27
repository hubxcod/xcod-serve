package stream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Signed app links.
//
// The app backend and xcod share app_secret. Without the secret nobody can create a link,
// change its channel or extend its expiry. There are three formats:
//
//  1. Start link (what the backend hands out when the user taps "Watch"):
//     /live/846.m3u8?st=EXP&sig=HMAC("start|846|EXP"), valid for about a minute.
//     xcod answers with a minimal master playlist pointing to the session; for .ts, it redirects.
//
//  2. Session (created by the start link, bound to the IP range of whoever opened it):
//     /live/846.m3u8?se=EXP&sig=HMAC("sess|846|EXP|NET"), valid for sessionTTL.
//     This is the URL the player reloads. Copied to another network, it does not work.
//     Segments get HMAC("segs|846|EXP|NET").
//
//  3. Legacy link (transition): /live/846.m3u8?exp=EXP&sig=HMAC("live|846|EXP"), no network;
//     segments with HMAC("seg|846|EXP").

// appKey identifies app viewers in counts and statistics.
const appKey = "app"

// sessionTTL is how long the session created by a start link lasts (a match, with room to spare).
const sessionTTL = 6 * time.Hour

func sign(secret string, parts ...string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strings.Join(parts, "|")))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func appSig(secret, scope, id string, exp int64) string {
	return sign(secret, scope, id, strconv.FormatInt(exp, 10))
}

func netSig(secret, scope, id string, exp int64, network string) string {
	return sign(secret, scope, id, strconv.FormatInt(exp, 10), network)
}

// SignAppLink returns the legacy-format query (exp=...&sig=...) that unlocks the channel until exp.
func SignAppLink(secret, id string, exp time.Time) string {
	e := exp.Unix()
	return "exp=" + strconv.FormatInt(e, 10) + "&sig=" + appSig(secret, "live", id, e)
}

// SignStartLink returns the start link query (st=...&sig=...).
func SignStartLink(secret, id string, exp time.Time) string {
	e := exp.Unix()
	return "st=" + strconv.FormatInt(e, 10) + "&sig=" + appSig(secret, "start", id, e)
}

// networkOf reduces the IP to the range used by sessions: /24 for IPv4 and /48 for IPv6. A
// range (not the exact IP) tolerates mobile carriers that change the public IP within the
// same network.
func networkOf(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d", v4[0], v4[1], v4[2])
	}
	return p.Mask(net.CIDRMask(48, 128)).String()
}

// validQuery checks a link's expiry and signature. param is the expiry field name (exp, st or
// se); an empty network means a link not bound to a network.
func validQuery(q url.Values, secret, param, scope, id, network string, now time.Time) (int64, bool) {
	if secret == "" {
		return 0, false
	}
	exp, err := strconv.ParseInt(q.Get(param), 10, 64)
	if err != nil || now.Unix() > exp {
		return 0, false
	}
	want := appSig(secret, scope, id, exp)
	if network != "" {
		want = netSig(secret, scope, id, exp, network)
	}
	return exp, hmac.Equal([]byte(q.Get("sig")), []byte(want))
}

// appQuery keeps the legacy signature (format 3), also used by the tests.
func appQuery(q url.Values, secret, scope, id string, now time.Time) (int64, bool) {
	return validQuery(q, secret, "exp", scope, id, "", now)
}

// appLink resolves a channel request made with an app link. It returns handled = true when it
// already answered (error, master playlist or redirect); otherwise it returns the app viewer.
func (h *Hub) appLink(w http.ResponseWriter, r *http.Request, id, ext string) (v viewer, handled bool) {
	cfg := h.cfg()
	q := r.URL.Query()
	ip := ClientIP(r, cfg)
	now := time.Now()
	reject := func(msg string) (viewer, bool) {
		log.Printf("app link rejected from %s: %s", ip, msg)
		http.Error(w, msg, http.StatusForbidden)
		return viewer{}, true
	}

	switch {
	case q.Has("st"): // start link -> session bound to the opener's network
		if _, ok := validQuery(q, cfg.AppSecret, "st", "start", id, "", now); !ok {
			return reject("invalid or expired link")
		}
		exp := now.Add(sessionTTL).Unix()
		// With repeaters, the session is created right on the server picked for this viewer.
		sess := h.pick() + "/live/" + id + ext + "?se=" + strconv.FormatInt(exp, 10) + "&sig=" + netSig(cfg.AppSecret, "sess", id, exp, networkOf(ip))
		w.Header().Set("Cache-Control", "no-store")
		if ext == ".ts" {
			http.Redirect(w, r, sess, http.StatusFound)
			return viewer{}, true
		}
		// Master playlist: the player reads it once and then only reloads the session URL.
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=3000000\n%s\n", sess)
		return viewer{}, true

	case q.Has("se"): // session
		network := networkOf(ip)
		exp, ok := validQuery(q, cfg.AppSecret, "se", "sess", id, network, now)
		if !ok {
			return reject("invalid or expired session, or from another network")
		}
		return viewer{ip: ip, ua: r.UserAgent(), key: appKey, appExp: exp, appNet: network}, false

	default: // legacy format
		exp, ok := validQuery(q, cfg.AppSecret, "exp", "live", id, "", now)
		if !ok {
			return reject("invalid or expired link")
		}
		return viewer{ip: ip, ua: r.UserAgent(), key: appKey, appExp: exp}, false
	}
}

// isAppRequest reports whether the query carries an app link (any format).
func isAppRequest(q url.Values) bool {
	return q.Has("sig") && (q.Has("st") || q.Has("se") || q.Has("exp"))
}

// signSegments signs the playlist's segment lines (those not starting with #). With a
// network, it uses the session signature; without one, the legacy format.
func signSegments(pl, secret, id string, exp int64, network string) string {
	e := strconv.FormatInt(exp, 10)
	q := "?exp=" + e + "&sig=" + appSig(secret, "seg", id, exp)
	if network != "" {
		q = "?se=" + e + "&sig=" + netSig(secret, "segs", id, exp, network)
	}
	lines := strings.Split(pl, "\n")
	for i, l := range lines {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines[i] = l + q
		}
	}
	return strings.Join(lines, "\n")
}

// validSegment checks the signature of a segment requested directly from xcod (not through
// the CDN).
func validSegment(q url.Values, secret, id, ip string, now time.Time) bool {
	if q.Has("se") {
		_, ok := validQuery(q, secret, "se", "segs", id, networkOf(ip), now)
		return ok
	}
	_, ok := validQuery(q, secret, "exp", "seg", id, "", now)
	return ok
}
