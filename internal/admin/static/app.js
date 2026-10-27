// Common panel behavior (no inline scripts: the CSP forbids them).
"use strict";

const $ = s => document.querySelector(s);
// csrf is the session's token (absent on the sign-in page: there is no session yet).
const csrf = () => document.querySelector('meta[name="csrf"]')?.content || "";
// base is the panel's secret path in front of /admin ("" when there is none).
const base = document.querySelector('meta[name="base"]')?.content || "";

// t translates a panel string (English is the key) and fills {name} placeholders.
const i18n = JSON.parse(document.getElementById("i18n")?.textContent || "{}");
function t(s, vars) {
  let out = i18n[s] || s;
  for (const k in vars || {}) out = out.replaceAll("{" + k + "}", vars[k]);
  return out;
}

// api makes JSON requests to the panel with the CSRF token.
async function api(path, body) {
  const opt = body === undefined ? {} : {
    method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf() }, body: JSON.stringify(body),
  };
  const r = await fetch(base + path, opt);
  if (r.status === 401) { location.href = base + "/admin/login"; throw new Error(t("session expired")); }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

document.addEventListener("submit", e => {
  const msg = e.target.dataset.confirm;
  if (msg && !confirm(msg)) e.preventDefault();
});

document.addEventListener("click", async e => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    try {
      await navigator.clipboard.writeText(copy.dataset.copy);
      const old = copy.textContent;
      copy.textContent = t("Copied!");
      setTimeout(() => copy.textContent = old, 1500);
    } catch { prompt(t("Copy the link:"), copy.dataset.copy); }
  }
  const gen = e.target.closest("[data-generate]");
  if (gen) {
    const b = crypto.getRandomValues(new Uint8Array(24));
    document.getElementById(gen.dataset.generate).value = Array.from(b, x => x.toString(16).padStart(2, "0")).join("");
  }
});

// language menu: switch as soon as another language is picked
document.addEventListener("change", e => {
  if (e.target.matches("[data-autosubmit]")) e.target.form.submit();
});
