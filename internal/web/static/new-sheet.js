// The new spreadsheet page: adds and removes tabs and columns, and numbers their inputs the way
// parseTabs reads them. Needs app.js loaded first.

const form = document.getElementById("new-sheet"), tabs = document.getElementById("tabs");
const tabTemplate = document.getElementById("tab-template"), columnTemplate = document.getElementById("column-template");

// renumber names each tab's inputs t<i>... and each column's t<i>c<j>..., in page order, and sets
// each key radio's value to its column's name.
function renumber() {
  tabs.querySelectorAll(".tab-card").forEach((card, i) => {
    const ti = `t${i}`;
    card.querySelector("[data-f=tab]").name = ti;
    card.querySelectorAll("[data-f=key]").forEach((radio) => { radio.name = ti + "key"; });
    card.querySelectorAll("tbody tr").forEach((row, j) => {
      const cj = `${ti}c${j}`, name = row.querySelector("[data-f=col]");
      name.name = cj;
      row.querySelector("[data-f=type]").name = cj + "type";
      row.querySelector("[data-f=required]").name = cj + "required";
      row.querySelector("[data-f=key]").value = name.value.trim();
    });
  });
}

form.addEventListener("click", (e) => {
  const button = e.target.closest("button");
  if (!button) return;
  const card = button.closest(".tab-card");
  if (button.id === "add-tab") {
    tabs.append(tabTemplate.content.cloneNode(true));
    renumber();
    tabs.lastElementChild.querySelector("[data-f=tab]").focus();
  } else if (button.hasAttribute("data-remove-tab")) {
    card.remove();
    renumber();
  } else if (button.hasAttribute("data-add-column")) {
    const body = card.querySelector("tbody");
    body.append(columnTemplate.content.cloneNode(true));
    renumber();
    body.lastElementChild.querySelector("[data-f=col]").focus();
  } else if (button.hasAttribute("data-remove-column")) {
    const row = button.closest("tr");
    if (row.querySelector("[data-f=key]").checked) card.querySelector("tfoot [data-f=key]").checked = true;
    row.remove();
    renumber();
  }
});
form.addEventListener("input", (e) => { if (e.target.matches("[data-f=col]")) renumber(); });
form.addEventListener("submit", renumber);
