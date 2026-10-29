// The columns page: picking the new name of a column that was renamed in Google Sheets copies its
// saved type, values, default, Required box and key choice onto the new column.

document.querySelectorAll("select.renamed").forEach((menu) => {
  menu.onchange = () => {
    if (!menu.value) return;
    const row = [...menu.closest(".card").querySelectorAll("tr[data-col]")].find((r) => r.dataset.col === menu.value);
    if (!row) return;
    const type = row.querySelector("select.col-type");
    type.value = menu.dataset.type;
    type.dispatchEvent(new Event("change", { bubbles: true }));
    row.querySelector(".enum-options").value = menu.dataset.options;
    row.querySelector("input[name$=default]").value = menu.dataset.default;
    row.querySelector("input[type=checkbox]").checked = menu.hasAttribute("data-required");
    if (menu.hasAttribute("data-key")) row.querySelector("input[type=radio]").checked = true;
  };
});
