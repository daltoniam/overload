(function () {
  // A form field named "action" shadows form.action, so read the attribute.
  function formAction(form) {
    return new URL(form.getAttribute('action') || '', document.baseURI).href;
  }
  var reduced = window.matchMedia('(prefers-reduced-motion: reduce)');
  var opener = null;
  function status(message) {
    var notice = document.getElementById('ui-status');
    if (notice) { notice.textContent = message; notice.hidden = !message; }
  }
  function closeDialog() {
    var dialog = document.getElementById('ui-dialog');
    if (!dialog || !dialog.open) return;
    if (dialog.dataset.dirty === 'true' && !window.confirm('Discard unsaved changes?')) return;
    dialog.classList.add('closing');
    window.setTimeout(function () { dialog.close(); dialog.classList.remove('closing'); dialog.dataset.dirty = 'false'; if (opener) opener.focus(); }, reduced.matches ? 0 : 160);
  }
  function initialize() {
    var theme = document.getElementById('theme-select');
    if (theme) theme.value = document.documentElement.dataset.theme || 'auto';
    document.querySelectorAll('form[data-enhance]').forEach(function (form) {
      form.setAttribute('hx-post', formAction(form));
      var errorTarget = form.closest('dialog') ? '#dialog-form-errors' : '#form-errors';
      form.setAttribute('hx-target', errorTarget);
      form.setAttribute('hx-swap', 'innerHTML');
      window.htmx.process(form);
    });
    document.querySelectorAll('.sub-agents:not([data-initialized])').forEach(function (list) {
      list.dataset.initialized = 'true';
      var extra = list.querySelectorAll('.sub-agent[data-extra]');
      var add = list.parentElement.querySelector('[data-add-sub-agent]');
      extra.forEach(function (slot) { slot.hidden = true; });
      if (add) add.hidden = extra.length === 0;
    });
    if (window.matchMedia('(max-width: 780px)').matches) {
      var nav = document.querySelector('.sidebar-details');
      if (nav && !nav.dataset.initialized) { nav.open = false; nav.dataset.initialized = 'true'; }
    }
  }
  document.addEventListener('change', function (event) {
    if (event.target.id === 'theme-select') {
      var choice = event.target.value;
      if (choice === 'auto') delete document.documentElement.dataset.theme;
      else document.documentElement.dataset.theme = choice;
      try { localStorage.setItem('overload-theme', choice); } catch (_) {}
    }
    if (event.target.id === 'wrap-lines') {
      var table = document.querySelector('.log-table');
      if (table) table.classList.toggle('wrap', event.target.checked);
    }
  });
  document.addEventListener('submit', function (event) {
    var form = event.target;
    if (!(form instanceof HTMLFormElement) || !formAction(form).endsWith('/delete') || form.querySelector('[name="confirmed"]')) return;
    event.preventDefault();
    opener = form.querySelector('button');
    var root = document.getElementById('modal-root');
    root.replaceChildren();
    var dialog = document.createElement('dialog');
    dialog.id = 'ui-dialog';
    dialog.setAttribute('aria-labelledby', 'delete-title');
    var body = document.createElement('div');
    body.className = 'dialog-body';
    var title = document.createElement('h2'); title.id = 'delete-title'; title.textContent = 'Confirm deletion'; body.appendChild(title);
    var description = document.createElement('p'); description.textContent = 'Delete ' + new URL(formAction(form)).pathname.replace('/delete', '').split('/').pop() + '? This cannot be undone. Dependent configuration or history may prevent removal.'; body.appendChild(description);
    var copy = form.cloneNode(true);
    var confirmed = document.createElement('input'); confirmed.type = 'hidden'; confirmed.name = 'confirmed'; confirmed.value = 'true'; copy.appendChild(confirmed);
    var cancel = document.createElement('button'); cancel.type = 'button'; cancel.className = 'secondary'; cancel.dataset.closeDialog = 'true'; cancel.textContent = 'Cancel'; copy.prepend(cancel); copy.classList.add('actions');
    body.appendChild(copy); dialog.appendChild(body); root.appendChild(dialog); dialog.showModal(); cancel.focus();
  });
  document.addEventListener('input', function (event) {
    var dialog = event.target.closest('dialog');
    if (dialog) dialog.dataset.dirty = 'true';
  });
  document.addEventListener('click', function (event) {
    if (event.target.closest('[data-close-dialog]')) { closeDialog(); return; }
    var move = event.target.closest('[data-move]');
    if (move) {
      var slot = move.closest('.workflow-slot');
      var neighbor = move.dataset.move === 'up' ? slot.previousElementSibling : slot.nextElementSibling;
      if (neighbor && neighbor.matches('.workflow-slot') && !neighbor.hidden) {
        var fields = slot.querySelectorAll('select, input, textarea');
        var others = neighbor.querySelectorAll('select, input, textarea');
        for (var i = 0; i < fields.length && i < others.length; i++) {
          var value = fields[i].value; fields[i].value = others[i].value; others[i].value = value;
        }
        var chips = slot.querySelector('.scope-chips'), otherChips = neighbor.querySelector('.scope-chips');
        if (chips && otherChips) { var html = chips.innerHTML; chips.innerHTML = otherChips.innerHTML; otherChips.innerHTML = html; }
        neighbor.querySelector('select').focus();
      }
    }
    var add = event.target.closest('[data-add-sub-agent]');
    if (add) {
      var hidden = add.parentElement.querySelectorAll('.sub-agent[hidden]');
      if (hidden.length) { hidden[0].hidden = false; hidden[0].querySelector('select').focus(); }
      add.hidden = hidden.length <= 1;
    }
    var pause = event.target.closest('[data-pause-events]');
    if (pause) { pause.dataset.paused = pause.dataset.paused === 'true' ? 'false' : 'true'; pause.textContent = pause.dataset.paused === 'true' ? 'Resume updates' : 'Pause updates'; }
  });
  document.addEventListener('cancel', function (event) { if (event.target.id === 'ui-dialog') { event.preventDefault(); closeDialog(); } }, true);
  document.addEventListener('htmx:beforeRequest', function (event) {
    if (event.detail.elt.matches('[data-open-model]')) opener = event.detail.elt;
    event.detail.target.setAttribute('aria-busy', 'true');
    if (event.detail.requestConfig.verb === 'post') {
      event.detail.elt.querySelectorAll('button[type="submit"]').forEach(function (button) { button.disabled = true; });
    }
    status('');
  });
  document.addEventListener('htmx:beforeSwap', function (event) {
    if (reduced.matches) event.detail.swapOverride = 'innerHTML swap:0ms settle:0ms';
    if (event.detail.xhr.getResponseHeader('X-UI-Validation') === 'true') { event.detail.shouldSwap = true; event.detail.isError = false; }
  });
  document.addEventListener('htmx:beforeTransition', function (event) { if (reduced.matches) event.preventDefault(); });
  document.addEventListener('htmx:afterSwap', function (event) {
    initialize();
    if (event.detail.target.id === 'run-events') {
      var marker = event.detail.target.querySelector('[data-run-active]');
      if (marker) event.detail.target.dataset.active = marker.dataset.runActive;
      var table = event.detail.target.querySelector('.log-table');
      var wrap = document.getElementById('wrap-lines');
      if (table && wrap) table.classList.toggle('wrap', wrap.checked);
    }
    if (event.detail.target.id === 'modal-root') {
      var dialog = document.getElementById('ui-dialog');
      if (dialog && !dialog.open) { dialog.showModal(); var field = dialog.querySelector('input:not([type="hidden"])'); if (field) field.focus(); }
    }
    var error = event.detail.target.id === 'dialog-form-errors' ? event.detail.target : document.getElementById('form-errors');
    if (error && error.textContent.trim()) { error.focus(); }
  });
  document.addEventListener('htmx:afterRequest', function (event) {
    if (event.detail.target) event.detail.target.removeAttribute('aria-busy');
    if (event.detail.elt) event.detail.elt.querySelectorAll('button[type="submit"]').forEach(function (button) { button.disabled = false; });
  });
  document.addEventListener('htmx:responseError', function () { status('The request could not be completed. Your previous content has been kept.'); });
  document.addEventListener('htmx:sendError', function () { status('Connection lost. Check the server and try again.'); });
  document.addEventListener('DOMContentLoaded', initialize);
  window.setInterval(function () {
    var region = document.getElementById('run-events');
    var pause = document.querySelector('[data-pause-events]');
    if (!region || region.dataset.active !== 'true' || document.hidden || (pause && pause.dataset.paused === 'true') || region.getAttribute('aria-busy') === 'true') return;
    var selection = window.getSelection();
    if (selection && !selection.isCollapsed) return;
    window.htmx.trigger(document.getElementById('event-search'), 'refreshEvents');
  }, 5000);
})();
