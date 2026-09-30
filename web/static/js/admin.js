// adsvc admin: the little the page needs beyond htmx. There is no inline script (the
// Content-Security-Policy allows none), so behaviour hangs off data attributes.
//
// Source of static/js/admin.js: run `make js` after editing.

// <input type="checkbox" data-select-all="name"> ticks every checkbox called name in
// its form; <button data-needs-selection="name"> is enabled while one of them is ticked.
function boxes(form, name) {
  return form ? Array.from(form.querySelectorAll(`input[type=checkbox][name="${name}"]`)) : [];
}

function updateSelection(form) {
  if (!form) return;
  for (const btn of form.querySelectorAll("[data-needs-selection]")) {
    btn.disabled = !boxes(form, btn.dataset.needsSelection).some((b) => b.checked);
  }
  for (const all of form.querySelectorAll("[data-select-all]")) {
    const bs = boxes(form, all.dataset.selectAll);
    const n = bs.filter((b) => b.checked).length;
    all.checked = bs.length > 0 && n === bs.length;
    all.indeterminate = n > 0 && n < bs.length;
  }
}

document.addEventListener("change", (e) => {
  const el = e.target;
  if (!(el instanceof HTMLInputElement) || el.type !== "checkbox") return;
  if (el.dataset.selectAll) {
    for (const b of boxes(el.form, el.dataset.selectAll)) b.checked = el.checked;
  }
  updateSelection(el.form);
});

// Swapped-in tables start with nothing selected.
document.addEventListener("htmx:afterSwap", (e) => {
  for (const f of e.target.querySelectorAll ? e.target.querySelectorAll("form") : []) updateSelection(f);
});

// <button data-copy="text"> copies text to the clipboard (the mpv commands of the
// Reports page).
document.addEventListener("click", async (e) => {
  const btn = e.target instanceof Element ? e.target.closest("[data-copy]") : null;
  if (!btn) return;
  const label = btn.textContent;
  let ok = false;
  try {
    await navigator.clipboard.writeText(btn.dataset.copy);
    ok = true;
  } catch {
    // no Clipboard API outside a secure context (plain HTTP on a LAN): the old way
    const ta = document.createElement("textarea");
    ta.value = btn.dataset.copy;
    document.body.append(ta);
    ta.select();
    ok = document.execCommand("copy");
    ta.remove();
  }
  btn.textContent = ok ? "✓" : "✗";
  setTimeout(() => { btn.textContent = label; }, 1500);
});
