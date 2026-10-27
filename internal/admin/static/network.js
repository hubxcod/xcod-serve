// Network and Server pages: refresh the status without reloading (forms stay intact).
"use strict";

const mb = v => (v || 0).toFixed(1) + " Mbps";
const sum = o => Object.values(o || {}).reduce((a, b) => a + b, 0);
const ago = t => {
  const s = (Date.now() - new Date(t)) / 1000;
  return s < 60 ? t("just now") : s < 3600 ? t("{n} min ago", { n: Math.floor(s / 60) }) : t("{n} h ago", { n: Math.floor(s / 3600) });
};
const zero = t => !t || t.startsWith("0001-");

function tag(text, cls) {
  const s = document.createElement("span");
  s.className = "tag " + cls; s.textContent = text;
  return s;
}

async function network() {
  let st;
  try { st = await api("/admin/api/network"); } catch { return; }
  const lm = $("#local-mbps");
  if (lm) lm.textContent = (st.local_mbps || 0).toFixed(1);
  for (const n of st.nodes || []) {
    const tr = document.querySelector(`tr[data-node="${n.id}"]`);
    if (!tr) continue;
    const off = tr.querySelector(".n-state .tag.off");
    if (!off) tr.querySelector(".n-state").replaceChildren(n.online ? tag(t("online"), "ok") : tag(zero(n.last_seen) ? t("waiting for install") : t("offline"), "warn"));
    tr.querySelector(".n-viewers").textContent = n.online ? sum(n.viewers) : "–";
    tr.querySelector(".n-band").textContent = n.online ? `${mb(n.out_mbps)} / ${n.capacity_mbps}` : "–";
    tr.querySelector(".n-url").textContent = n.public_url || "–";
    tr.querySelector(".n-upd").textContent = n.outdated ? t("outdated") + (n.update ? ": " + n.update : "") : "";
  }
  const a = st.agent;
  if (a && $("#ag-state")) {
    $("#ag-state").textContent = a.last_err ? t("error") : zero(a.last_ok) ? t("connecting…") : t("connected");
    $("#ag-last").textContent = zero(a.last_ok) ? "–" : t("last contact {when}", { when: ago(a.last_ok) });
    $("#ag-name").textContent = a.name || "–";
    $("#ag-ch").textContent = t("{n} channel(s)", { n: a.channels });
    $("#ag-err").textContent = a.last_err || "";
    $("#ag-err").hidden = !a.last_err;
    $("#ag-upd").textContent = t("Update: {status}", { status: a.update || "" });
    $("#ag-upd").hidden = !a.update;
  }
}

async function certificate() {
  let st;
  try { st = await api("/admin/api/server"); } catch { return; }
  $("#c-state").textContent = st.checking ? t("issuing…") : st.ok ? t("valid") : t("error");
  $("#c-exp").textContent = st.ok ? t("until {date}", { date: new Date(st.expires).toISOString().slice(0, 10) }) : "–";
  $("#c-dns").textContent = (st.dns || []).join(" ") || "–";
  $("#c-err").textContent = st.err || "";
  $("#c-err").hidden = !st.err;
  return st.checking;
}

if (document.querySelector("tr[data-node]") || $("#ag-state")) { network(); setInterval(network, 4000); }
if ($("#c-state")) {
  // while issuing, check every 2s; then stop
  const t = setInterval(async () => { if (!(await certificate())) clearInterval(t); }, 2000);
}
