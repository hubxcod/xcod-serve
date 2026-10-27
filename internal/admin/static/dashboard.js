// Live dashboard: refreshes every 2s from /admin/api/live.
"use strict";

let prev = {}, prevT = 0, hls = null, holding = false;
document.addEventListener("pointerdown", () => holding = true);
document.addEventListener("pointerup", () => holding = false);
document.addEventListener("pointercancel", () => holding = false);

const nf0 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });
const mbps = (bytes, secs) => secs > 0 ? (bytes * 8 / secs / 1e6).toFixed(1) + " Mbps" : "–";

// el creates an element with text (always textContent: no HTML from data).
function el(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (cls) e.className = cls;
  return e;
}

function link(href, text) {
  const a = el("a", text);
  a.href = href; a.target = "_blank"; a.rel = "noopener";
  return a;
}

async function refresh() {
  let st;
  try { st = await api("/admin/api/live"); } catch { $("#sum").textContent = t("cannot reach the server"); return; }
  const now = performance.now(), dt = prevT ? (now - prevT) / 1000 : 0;
  let ts = 0, hl = 0, up = 0, sumSrc = 0, sumUp = 0;
  const rows = [];
  for (const s of st.channels) {
    ts += s.ts; hl += s.hls; if (s.running) up++;
    const p = prev[s.id], known = p && dt > 0, ok = s.running && known;
    const src = ok ? s.bytes_src - p.src : 0, out = ok ? s.bytes_in - p.in : 0;
    const egress = known ? Math.max(0, s.bytes_out - p.out) : 0;
    prev[s.id] = { src: s.bytes_src, in: s.bytes_in, out: s.bytes_out };
    sumSrc += src; sumUp += egress;

    const tr = el("tr");
    const name = el("td"); name.append(el("b", s.name), el("div", s.id === s.name ? s.quality : `${s.id} · ${s.quality}`, "q"));
    const state = el("td"); state.append(el("span", s.running ? t("live") : t("idle"), "tag " + (s.running ? "ok" : "off")));
    const signal = el("td", ok ? mbps(out, dt) : "–", "num");
    if (ok && src > 0 && out < src * 0.95) signal.append(el("div", t("−{n}% per viewer", { n: Math.round((1 - out / src) * 100) }), "eco"));
    const links = el("td", undefined, "links");
    if (!st.require_key) {
      links.append(link(`${st.base}/live/${encodeURIComponent(s.id)}.ts`, ".ts"),
                   link(`${st.hls_base}/live/${encodeURIComponent(s.id)}.m3u8`, st.hls_base !== st.base ? ".m3u8 (CDN)" : ".m3u8"));
    }
    const play = el("button", t("Watch"), "small"); play.type = "button"; play.dataset.play = s.id;
    links.append(play);
    const act = el("td");
    const kick = el("button", t("Kick"), "small danger"); kick.type = "button"; kick.dataset.kick = s.id;
    kick.disabled = !(s.ts + s.hls);
    act.append(kick);
    tr.append(name, state, el("td", s.ts, "num"), el("td", s.hls, "num"), el("td", ok ? mbps(src, dt) : "–", "num"), signal,
      el("td", known && (s.running || egress) ? mbps(egress, dt) : "–", "num"), el("td", s.uptime || "–", "num hide-sm"), links, act);
    rows.push(tr);
    if (s.viewers && s.viewers.length) {
      const vt = el("tr"), cell = el("td"); cell.colSpan = 9;
      cell.append(el("div", s.viewers.map(v => `${v.kind.padEnd(3)} ${v.ip}${v.key ? "  · " + v.key : ""}`).join("\n"), "viewers"));
      vt.append(el("td"), cell);
      rows.push(vt);
    }
  }
  prevT = now;

  const u = st.usage;
  $("#t-viewers").textContent = nf0.format(ts + hl);
  $("#t-viewers-d").textContent = `${ts} TS · ${hl} HLS`;
  $("#t-cpu").textContent = nf0.format(u.cpu) + "%";
  $("#t-cpu-d").textContent = t("max. {max}% ({cores} cores) · {ffmpeg} ffmpeg", { max: u.cores * 100, cores: u.cores, ffmpeg: u.ffmpeg });
  $("#t-mem").textContent = nf0.format(u.mem_mb) + " MB";
  $("#t-up-n").textContent = t("{n} provider connection(s)", { n: up });
  if (dt > 0) {
    $("#t-down").textContent = mbps(sumSrc, dt);
    $("#t-up").textContent = mbps(sumUp, dt);
    $("#t-ratio").textContent = sumSrc > 0 ? t("×{n} the ingress", { n: (sumUp / sumSrc).toFixed(1) }) : "–";
  }
  $("#key-hint").hidden = !st.require_key;
  $("#sum").textContent = t("{viewers} viewer(s) · {up} provider connection(s)", { viewers: ts + hl, up });
  if (holding) return; // redrawing between press and release would lose the click
  const body = $("#live");
  if (rows.length) body.replaceChildren(...rows);
  else { const tr = el("tr"), td = el("td", t("No channels. Add them under Channels."), "muted"); td.colSpan = 10; tr.append(td); body.replaceChildren(tr); }
}

function play(id) {
  const v = $("#player"), src = base + "/admin/preview/" + encodeURIComponent(id) + ".m3u8";
  v.hidden = false;
  if (hls) { hls.destroy(); hls = null; }
  if (window.Hls && Hls.isSupported()) { hls = new Hls({ liveSyncDurationCount: 3 }); hls.loadSource(src); hls.attachMedia(v); }
  else v.src = src; // Safari plays HLS natively
  v.play().catch(() => {});
}

document.addEventListener("click", async e => {
  const k = e.target.dataset.kick, p = e.target.dataset.play;
  if (p) play(p);
  if (k && confirm(t("Kick all viewers of channel {id}? They cannot come back for 1 minute.", { id: k }))) {
    try { await api("/admin/api/kick", { channel: k }); refresh(); } catch (err) { alert(err.message); }
  }
});

refresh();
setInterval(refresh, 2000);
