// The spreadsheets page: tab ops and adding a spreadsheet with Google Picker. Needs app.js and
// Google's api.js loaded first.

// A spreadsheet's Save ops button lights up once any of its tabs' ops change.
document.querySelectorAll("form.ops-form").forEach((f) => {
  const button = f.querySelector("button");
  for (const input of f.elements) input.addEventListener("input", () => button.classList.add("dirty"));
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
