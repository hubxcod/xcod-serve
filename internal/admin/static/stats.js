// Statistics: totals per period, 24h charts and tables (/admin/api/stats).
"use strict";

let statsData = null, period = 1;
const nf1 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 });
const nf0 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });

function bytes(n) {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
  return (i ? nf1.format(n) : nf0.format(n)) + " " + u[i];
}
const ratio = (up, down) => down > 0 ? "×" + nf1.format(up / down) : "–";
const hours = min => nf1.format(min / 60) + " h";
const localDay = d => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;

// days in the period (today and the N-1 before it)
function periodDays(n) {
  const out = [], d = new Date();
  for (let i = 0; i < n; i++) { out.push(localDay(d)); d.setDate(d.getDate() - 1); }
  return out;
}

function sumDay(chans) {
  const t = { down: 0, up: 0, peak: 0, min: 0 };
  for (const c of Object.values(chans || {})) { t.down += c.down; t.up += c.up; t.peak = Math.max(t.peak, c.peak_viewers); t.min += c.viewer_minutes; }
  return t;
}

// row builds a table row with text only.
function row(cells) {
  const tr = document.createElement("tr");
  cells.forEach(([text, cls, bold]) => {
    const td = document.createElement("td");
    if (cls) td.className = cls;
    if (bold) { const b = document.createElement("b"); b.textContent = text; td.append(b); } else td.textContent = text;
    tr.append(td);
  });
  return tr;
}
function emptyRow(text) {
  const tr = document.createElement("tr"), td = document.createElement("td");
  td.colSpan = 6; td.className = "muted"; td.textContent = text; tr.append(td);
  return tr;
}

async function loadStats() {
  try { statsData = await api("/admin/api/stats"); } catch { return; }
  $("#t-since-up").textContent = "↑ " + bytes(statsData.total_up);
  $("#t-since").textContent = t("↓ {n} from the provider", { n: bytes(statsData.total_down) });
  renderPeriod();
  renderCharts();
}

function renderPeriod() {
  if (!statsData) return;
  const days = periodDays(period), name = { 1: t("today"), 7: t("7 days"), 30: t("30 days"), 90: t("90 days") }[period];
  const tot = { down: 0, up: 0, peak: 0, min: 0 }, byCh = {};
  for (const day of days) {
    for (const [id, c] of Object.entries(statsData.days[day] || {})) {
      const b = byCh[id] ||= { down: 0, up: 0, peak: 0, min: 0 };
      b.down += c.down; b.up += c.up; b.peak = Math.max(b.peak, c.peak_viewers); b.min += c.viewer_minutes;
    }
    tot.peak = Math.max(tot.peak, sumDay(statsData.days[day]).peak);
  }
  for (const b of Object.values(byCh)) { tot.down += b.down; tot.up += b.up; tot.min += b.min; }

  $("#t-p-label").textContent = period === 1 ? t("Downloaded today") : t("Downloaded, {period}", { period: name });
  $("#t-p-label2").textContent = period === 1 ? t("Sent today") : t("Sent, {period}", { period: name });
  $("#t-p-down").textContent = bytes(tot.down);
  $("#t-p-up").textContent = bytes(tot.up);
  $("#t-p-ratio").textContent = tot.down > 0 ? t("{n} what was downloaded", { n: ratio(tot.up, tot.down) }) : t("to viewers");
  $("#t-p-hours").textContent = hours(tot.min);
  $("#t-p-peak").textContent = t("peak of {n} concurrent viewer(s)", { n: tot.peak });

  $("#ch-title").textContent = t("Per channel · {period}", { period: name });
  const ids = Object.keys(byCh).sort((a, b) => byCh[b].up - byCh[a].up);
  $("#by-channel").replaceChildren(...(ids.length ? ids.map(id => {
    const b = byCh[id];
    return row([[id, "", true], [bytes(b.down), "num"], [bytes(b.up), "num"], [ratio(b.up, b.down), "num"], [b.peak, "num"], [hours(b.min), "num"]]);
  }) : [emptyRow(t("No data in this period."))]));

  const allDays = Object.keys(statsData.days).sort().reverse();
  $("#by-day").replaceChildren(...(allDays.length ? allDays.map(day => {
    const t = sumDay(statsData.days[day]);
    return row([[day], [bytes(t.down), "num"], [bytes(t.up), "num"], [ratio(t.up, t.down), "num"], [t.peak, "num"], [hours(t.min), "num"]]);
  }) : [emptyRow(t("No data yet: the first minute is recorded 1 min after starting."))]));
}

function renderCharts() {
  if (!statsData) return;
  const ser = statsData.series, mb = b => b * 8 / 60 / 1e6;
  lineChart($("#c-band"), ser, [
    { name: t("Ingress"), color: "var(--s-down)", get: p => mb(p.down) },
    { name: t("Egress"), color: "var(--s-up)", get: p => mb(p.up) },
  ], v => nf1.format(v) + " Mbps");
  lineChart($("#c-viewers"), ser, [{ name: t("Viewers"), color: "var(--s-viewers)", get: p => p.viewers }], v => nf0.format(v), true);
  lineChart($("#c-cpu"), ser, [{ name: "CPU", color: "var(--s-cpu)", get: p => p.cpu }], v => nf0.format(v) + "%");
}

function niceMax(v, integer) {
  if (v <= 0) return integer ? 4 : 1;
  const p = Math.pow(10, Math.floor(Math.log10(v))), n = v / p;
  const m = (n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10) * p;
  return integer ? Math.max(4, Math.ceil(m / 4) * 4) : m;
}

// lineChart draws 24h series in SVG, with a crosshair and tooltip.
// Colors go through .style (the CSP blocks style attributes in HTML).
function lineChart(box, pts, series, fmt, integer) {
  box.querySelector("svg")?.remove();
  box.querySelector(".tip")?.remove();
  const NS = "http://www.w3.org/2000/svg", W = Math.max(box.clientWidth, 300), H = 170;
  const L = 40, R = 12, T = 8, B = 22, iw = W - L - R, ih = H - T - B;
  const svg = document.createElementNS(NS, "svg");
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`); svg.setAttribute("height", H);
  svg.setAttribute("role", "img"); svg.setAttribute("aria-label", box.querySelector("h3").textContent);
  const el = (tag, attrs, style) => {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    for (const k in style || {}) e.style.setProperty(k, style[k]);
    svg.appendChild(e);
    return e;
  };

  const t1 = Date.now() / 1000, t0 = t1 - 86400;
  const data = pts.filter(p => p.t >= t0);
  const ymax = niceMax(Math.max(0, ...data.flatMap(p => series.map(s => s.get(p)))), integer);
  const x = t => L + (t - t0) / 86400 * iw, y = v => T + ih - v / ymax * ih;

  for (let i = 0; i <= 4; i++) {
    const v = ymax * i / 4, yy = y(v);
    el("line", { x1: L, x2: W - R, y1: yy, y2: yy, class: "gl" });
    el("text", { x: L - 6, y: yy + 4, "text-anchor": "end", class: "axis" }).textContent = nf1.format(v); // unit in the title
  }
  const h0 = new Date(t0 * 1000); h0.setMinutes(0, 0, 0);
  for (let d = new Date(h0); d.getTime() / 1000 <= t1; d.setHours(d.getHours() + 1)) {
    if (d.getHours() % 4 || d.getTime() / 1000 < t0) continue;
    el("text", { x: x(d.getTime() / 1000), y: H - 6, "text-anchor": "middle", class: "axis" }).textContent = String(d.getHours()).padStart(2, "0") + "h";
  }
  if (!data.length) {
    el("text", { x: L + iw / 2, y: T + ih / 2, "text-anchor": "middle", class: "empty" }).textContent = t("no data yet: the first point shows up 1 min after starting");
    box.appendChild(svg);
    return;
  }
  for (const s of series) {
    let d = "";
    data.forEach((p, i) => { // break the line where the server was down
      const joinPrev = i > 0 && p.t - data[i - 1].t <= 150, joinNext = i < data.length - 1 && data[i + 1].t - p.t <= 150;
      d += (joinPrev ? "L" : "M") + x(p.t).toFixed(1) + " " + y(s.get(p)).toFixed(1);
      if (!joinPrev && !joinNext) el("circle", { cx: x(p.t), cy: y(s.get(p)), r: 3 }, { fill: s.color }); // isolated point
    });
    el("path", { d, class: "line" }, { stroke: s.color });
  }

  const xh = el("line", { y1: T, y2: T + ih, class: "xh", visibility: "hidden" });
  const dots = series.map(s => el("circle", { r: 4, visibility: "hidden" }, { fill: s.color, stroke: "var(--card)", "stroke-width": "2" }));
  const hit = el("rect", { x: L, y: T, width: iw, height: ih, fill: "transparent" });
  const tip = document.createElement("div"); tip.className = "tip"; box.appendChild(tip);
  box.appendChild(svg);

  const hide = () => { tip.style.display = "none"; xh.setAttribute("visibility", "hidden"); dots.forEach(c => c.setAttribute("visibility", "hidden")); };
  hit.addEventListener("pointerleave", hide);
  hit.addEventListener("pointermove", e => {
    const r = svg.getBoundingClientRect(), px = (e.clientX - r.left) * W / r.width;
    const t = t0 + (px - L) / iw * 86400;
    let lo = 0, hi = data.length - 1;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (data[mid].t < t) lo = mid + 1; else hi = mid; }
    if (lo > 0 && t - data[lo - 1].t < data[lo].t - t) lo--;
    const p = data[lo], xx = x(p.t);
    xh.setAttribute("x1", xx); xh.setAttribute("x2", xx); xh.setAttribute("visibility", "visible");
    series.forEach((s, i) => { dots[i].setAttribute("cx", xx); dots[i].setAttribute("cy", y(s.get(p))); dots[i].setAttribute("visibility", "visible"); });
    tip.replaceChildren();
    const head = document.createElement("div"); head.className = "t";
    head.textContent = new Date(p.t * 1000).toLocaleString("en-US", { weekday: "short", hour: "2-digit", minute: "2-digit", hour12: false });
    tip.appendChild(head);
    series.forEach(s => {
      const line = document.createElement("div"), key = document.createElement("i"), val = document.createElement("b"), nm = document.createElement("span");
      key.style.background = s.color; val.textContent = fmt(s.get(p)); nm.textContent = s.name;
      line.append(key, val, nm); tip.appendChild(line);
    });
    tip.style.display = "block";
    const bx = box.getBoundingClientRect(), left = xx * r.width / W + (r.left - bx.left);
    tip.style.left = Math.min(Math.max(0, left + 12), bx.width - tip.offsetWidth) + "px";
    tip.style.top = (r.top - bx.top + 4) + "px";
  });
}

$("#period").addEventListener("click", e => {
  const p = Number(e.target.dataset.p);
  if (!p) return;
  period = p;
  document.querySelectorAll("#period button").forEach(b => b.classList.toggle("on", Number(b.dataset.p) === p));
  renderPeriod();
});

let resizeT;
window.addEventListener("resize", () => { clearTimeout(resizeT); resizeT = setTimeout(renderCharts, 150); });
loadStats();
setInterval(loadStats, 30000);
