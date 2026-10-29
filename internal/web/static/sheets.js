// The spreadsheets page: tab rows and adding a spreadsheet with Google Picker. Needs app.js and
// Google's api.js loaded first.

// A tab row's Save button lights up once its ops, hidden columns or key column change.
document.querySelectorAll("form.tab-form").forEach((f) => {
  const button = f.querySelector("button");
  for (const input of f.elements) input.addEventListener("input", () => button.classList.add("dirty"));
});

// A tab row's preset menu sets its op checkboxes, and follows them: ticking a box by hand
// switches the menu to the preset that matches, or to Custom.
document.querySelectorAll("select.ops-preset").forEach((menu) => {
  const boxes = [...document.querySelectorAll(`input[name=ops][form="${menu.dataset.form}"]`)];
  menu.onchange = () => {
    const ops = menu.value.split(",");
    for (const box of boxes) {
      if (box.checked === ops.includes(box.value)) continue;
      box.checked = !box.checked;
      box.dispatchEvent(new Event("input", { bubbles: true })); // lights up Save
    }
  };
  const follow = () => {
    const ops = boxes.filter((b) => b.checked).map((b) => b.value).join(",");
    menu.value = [...menu.options].some((o) => o.value === ops && !o.disabled) ? ops : "custom";
  };
  for (const box of boxes) box.addEventListener("change", follow);
});

const add = document.getElementById("add"), pick = document.getElementById("pick"), pickArea = document.getElementById("pick-area");
document.getElementById("add-cancel").onclick = () => { add.hidden = true; pickArea.hidden = false; };

pick.onclick = async () => {
  const res = await fetch("/app/token");
  const body = await res.json();
  if (!res.ok) { showError(body.error); return; }
  gapi.load("picker", () => {
    new google.picker.PickerBuilder()
      .addView(new google.picker.DocsView(google.picker.ViewId.SPREADSHEETS))
      .setOAuthToken(body.token)
      .setDeveloperKey(pick.dataset.pickerKey)
      .setAppId(pick.dataset.appId)
      .setCallback((data) => {
        if (data.action !== google.picker.Action.PICKED) return;
        const doc = data.docs[0];
        add.google_id.value = doc.id;
        add.title.value = doc.name;
        add.name.value = doc.name.replace(/[^A-Za-z0-9_-]+/g, "-").replace(/^-|-$/g, "").toLowerCase();
        document.getElementById("picked").textContent = doc.name;
        add.hidden = false;
        pickArea.hidden = true;
        add.name.focus();
      })
      .build()
      .setVisible(true);
  });
};
