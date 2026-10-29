// Behavior every signed-in page shares.

const flash = document.getElementById("flash");
function showError(msg) {
  const p = document.createElement("p");
  p.className = "flash error"; p.setAttribute("role", "alert"); p.textContent = msg;
  flash.replaceChildren(p);
  p.scrollIntoView({ block: "nearest" });
}

document.querySelectorAll(".origin").forEach((el) => { el.textContent = location.origin; });

document.querySelectorAll("[data-copy]").forEach((btn) => {
  btn.onclick = async () => {
    try {
      await navigator.clipboard.writeText(document.querySelector(btn.dataset.copy).textContent.trim());
      btn.textContent = "Copied";
      setTimeout(() => { btn.textContent = "Copy"; }, 1500);
    } catch { showError("Could not copy; select the text and copy it instead."); }
  };
});

document.querySelectorAll("form[data-confirm]").forEach((f) => {
  f.onsubmit = (e) => { if (!confirm(f.dataset.confirm)) e.preventDefault(); };
});

// A column's values box shows while its type menu says One of (enum), filled in from the column's
// distinct values if it's empty. Delegated, so rows added later work too.
document.addEventListener("change", (e) => {
  if (!e.target.matches("select.col-type")) return;
  const box = e.target.parentElement.querySelector(".enum-options");
  box.hidden = e.target.value !== "enum";
  if (!box.hidden && !box.value) box.value = box.dataset.values || "";
});
