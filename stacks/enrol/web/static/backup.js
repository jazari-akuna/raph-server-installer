/* enrol /backup — restore-modal opener, copy-to-clipboard, SSE progress.
   Vanilla; loaded with `defer` so DOM is ready when this runs. */
(function () {
  // Restore modal — set the snapshot hidden input from the clicked
  // button's data-snapshot attribute (empty string == latest snapshot,
  // server-side default).
  document.querySelectorAll('button.restore-open').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var modal = document.getElementById(btn.dataset.modal);
      if (!modal) return;
      var snap = btn.dataset.snapshot || '';
      var input = modal.querySelector('input[name="snapshot"]');
      if (input) input.value = snap;
      var confirmField = modal.querySelector('input[name="confirm"]');
      if (confirmField) confirmField.value = '';
      if (typeof modal.showModal === 'function') {
        modal.showModal();
      } else {
        modal.setAttribute('open', '');
      }
      if (confirmField) confirmField.focus();
    });
  });

  // Copy-to-clipboard for off-host command blocks. Flashes "Copied!"
  // for ~1.2s then restores the original label.
  document.querySelectorAll('button.copy-btn').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var pre = btn.parentElement && btn.parentElement.querySelector('pre.cmd');
      if (!pre) return;
      var orig = btn.textContent;
      navigator.clipboard.writeText(pre.textContent).then(function () {
        btn.textContent = 'Copied!';
        setTimeout(function () { btn.textContent = orig; }, 1200);
      }, function () {
        btn.textContent = 'Copy failed';
        setTimeout(function () { btn.textContent = orig; }, 1500);
      });
    });
  });

  // SSE progress consumer — only when a backup/restore is in flight.
  var prog = document.getElementById('backup-progress');
  if (!prog) return;
  var op = prog.dataset.op;
  if (!op) return;
  var stepEl = prog.querySelector('.step');
  var logEl = prog.querySelector('.log');
  var es = new EventSource('/backup/events?op=' + encodeURIComponent(op));
  es.addEventListener('status', function (ev) {
    try {
      var d = JSON.parse(ev.data);
      if (stepEl) stepEl.textContent = d.step ? (d.step + ': ' + (d.msg || '')) : (d.msg || ev.data);
    } catch (e) { if (stepEl) stepEl.textContent = ev.data; }
  });
  es.addEventListener('log', function (ev) {
    if (!logEl) return;
    try {
      var d = JSON.parse(ev.data);
      logEl.textContent += (d.line || ev.data) + '\n';
    } catch (e) { logEl.textContent += ev.data + '\n'; }
    logEl.scrollTop = logEl.scrollHeight;
  });
  es.addEventListener('done', function () {
    es.close();
    window.location.reload();
  });
  es.addEventListener('error', function (ev) {
    if (ev.data) {
      try {
        var d = JSON.parse(ev.data);
        if (stepEl) { stepEl.textContent = 'failed: ' + (d.msg || ''); stepEl.classList.add('err'); }
      } catch (e) { if (logEl) logEl.textContent += '[error] ' + ev.data + '\n'; }
      es.close();
    }
  });
})();
