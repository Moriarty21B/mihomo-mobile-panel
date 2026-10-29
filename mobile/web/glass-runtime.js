/* One optical navigation lens. Movement updates transforms, never its canvas. */
(function () {
  'use strict';
  const root = document.documentElement;
  const app = document.getElementById('app');
  const nav = document.getElementById('glassNav');
  if (!app || !nav) return;
  const capsule = nav.querySelector(':scope > .nav-capsule');
  const buttons = Array.from(nav.querySelectorAll('button[data-page]'));
  if (!capsule || !buttons.length) return;
  const media = {
    motion: matchMedia('(prefers-reduced-motion: reduce)'),
    transparency: matchMedia('(prefers-reduced-transparency: reduce)'),
    contrast: matchMedia('(prefers-contrast: more)'),
    forced: matchMedia('(forced-colors: active)'),
    coarse: matchMedia('(pointer: coarse)')
  };
  let lens = null, lensKey = '', refreshFrame = 0, movementFrame = 0;
  let lastTime = 0, settledSince = 0, placed = false;
  let pointer = null, suppressClick = null, selectedButton = null, width = 66;
  const height = 56;
  const values = { x: 0, y: 0, sx: 1, sy: 1, gx: 36, gy: 12 };
  const target = { ...values };
  const velocity = { x: 0, y: 0, sx: 0, sy: 0, gx: 0, gy: 0 };
  const clamp = (value, min, max) => Math.max(min, Math.min(max, value));
  function classState(element, name, state) {
    if (element.classList.contains(name) !== state) element.classList.toggle(name, state);
  }
  function visible() {
    return !document.hidden && !app.classList.contains('hidden') && nav.getClientRects().length > 0;
  }
  function reducedMaterial() {
    const weakPhone = media.coarse.matches && (
      (typeof navigator.deviceMemory === 'number' && navigator.deviceMemory <= 4) ||
      (typeof navigator.hardwareConcurrency === 'number' && navigator.hardwareConcurrency <= 4)
    );
    return root.classList.contains('reduced-effects') || media.transparency.matches ||
      media.contrast.matches || media.forced.matches ||
      Boolean(navigator.connection && navigator.connection.saveData) || weakPhone;
  }
  function noMotion() { return media.motion.matches || reducedMaterial(); }
  function activeButton() { return buttons.find(button => button.classList.contains('active')) || buttons[0]; }
  function buttonPoint(button) {
    return { x: button.offsetLeft + button.offsetWidth / 2 - width / 2,
      y: button.offsetTop + button.offsetHeight / 2 - height / 2 };
  }
  function closestButton(localX) {
    return buttons.reduce((nearest, button) => {
      const distance = Math.abs(button.offsetLeft + button.offsetWidth / 2 - localX);
      const nearestDistance = Math.abs(nearest.offsetLeft + nearest.offsetWidth / 2 - localX);
      return distance < nearestDistance ? button : nearest;
    }, buttons[0]);
  }
  function preview(button) {
    for (const item of buttons) classState(item, 'is-preview', item === button);
  }
  function draw() {
    capsule.style.setProperty('--nav-x', values.x.toFixed(3) + 'px');
    capsule.style.setProperty('--nav-y', values.y.toFixed(3) + 'px');
    capsule.style.setProperty('--lens-sx', values.sx.toFixed(4));
    capsule.style.setProperty('--lens-sy', values.sy.toFixed(4));
    capsule.style.setProperty('--glint-x', values.gx.toFixed(2) + '%');
    capsule.style.setProperty('--glint-y', values.gy.toFixed(2) + '%');
  }
  function finishMovement() {
    cancelAnimationFrame(movementFrame);
    movementFrame = 0; lastTime = 0;
    for (const name of Object.keys(values)) { values[name] = target[name]; velocity[name] = 0; }
    draw(); classState(nav, 'is-settling', false);
  }
  function stepMovement(time) {
    movementFrame = 0;
    if (!visible() || noMotion()) { finishMovement(); return; }
    const dt = Math.min((time - (lastTime || time - 16)) / 1000, 0.032);
    lastTime = time;
    let settled = true;
    // Exact critically damped integration stays quick even on a slow frame.
    for (const name of Object.keys(values)) {
      const omega = name === 'x' ? 38 : 42;
      const delta = values[name] - target[name];
      const impulse = velocity[name] + omega * delta;
      const decay = Math.exp(-omega * dt);
      values[name] = target[name] + (delta + impulse * dt) * decay;
      velocity[name] = (velocity[name] - impulse * omega * dt) * decay;
      const epsilon = name === 'sx' || name === 'sy' ? 0.0008 : 0.12;
      if (Math.abs(values[name] - target[name]) > epsilon) settled = false;
    }
    draw();
    // A held press and an idle dock never leave an animation loop running.
    if (settled || (!pointer && time - settledSince >= 260)) finishMovement();
    else movementFrame = requestAnimationFrame(stepMovement);
  }
  function moveTo(next, immediate) {
    Object.assign(target, next);
    if (immediate || noMotion() || !placed) { finishMovement(); return; }
    if (!pointer) { settledSince = performance.now(); classState(nav, 'is-settling', true); }
    if (!movementFrame) { lastTime = 0; movementFrame = requestAnimationFrame(stepMovement); }
  }
  function restAtSelection(immediate) {
    moveTo({ ...buttonPoint(activeButton()), sx: 1, sy: 1, gx: 36, gy: 12 }, immediate);
  }
  function releaseCapture(id) {
    try { if (nav.hasPointerCapture(id)) nav.releasePointerCapture(id); }
    catch (_) { /* A browser may already have released a touch capture. */ }
  }
  function blockCompatibilityClick(id) { suppressClick = { id, until: performance.now() + 450 }; }
  function cancelGesture(immediate, suppress) {
    const previous = pointer;
    pointer = null;
    if (previous) {
      if (suppress || previous.dragging) blockCompatibilityClick(previous.id);
      releaseCapture(previous.id);
    }
    classState(nav, 'is-pressed', false); classState(nav, 'is-dragging', false); preview(null);
    if (placed) restAtSelection(immediate);
  }
  function syncNavigation() {
    if (!visible()) return;
    const selected = activeButton();
    if (!selected.offsetWidth || !selected.offsetHeight) return;
    const first = !placed, changed = selected !== selectedButton;
    const nextWidth = Math.min(selected.offsetWidth + 4, 66);
    const geometryChanged = Math.abs(nextWidth - width) > 0.1;
    if (geometryChanged && pointer) cancelGesture(true, true);
    width = nextWidth;
    capsule.style.width = width + 'px'; capsule.style.height = height + 'px';
    for (const button of buttons) {
      if (button === selected) {
        if (button.getAttribute('aria-current') !== 'page') button.setAttribute('aria-current', 'page');
      } else if (button.hasAttribute('aria-current')) button.removeAttribute('aria-current');
    }
    selectedButton = selected;
    if (changed && pointer) cancelGesture(false, true);
    if (!pointer && (first || changed || geometryChanged)) restAtSelection(first || geometryChanged);
    placed = true; classState(nav, 'glass-ready', true);
  }
  function destroyLens() {
    if (lens) lens.destroy();
    lens = null; lensKey = ''; classState(nav, 'liquid-glass-active', false);
  }
  function refreshNow() {
    refreshFrame = 0;
    const reduced = reducedMaterial();
    classState(root, 'glass-reduced', reduced); classState(root, 'motion-reduced', media.motion.matches);
    if (!visible()) {
      cancelGesture(true, true); finishMovement(); destroyLens(); placed = false; return;
    }
    syncNavigation(); restAtSelection(true);
    const api = window.LiquidGlass;
    const canvasSupported = typeof CanvasRenderingContext2D !== 'undefined' &&
      typeof CanvasRenderingContext2D.prototype.roundRect === 'function';
    if (reduced || media.motion.matches || !api || !api.isChromium || !canvasSupported) {
      destroyLens(); nav.dataset.glass = reduced || media.motion.matches ? 'reduced' : 'blur'; return;
    }
    const dark = root.dataset.theme === 'dark', radius = height / 2;
    const key = [Math.round(width), height, radius, dark].join(':');
    if (lens && lensKey === key) return;
    const options = {
      filterId: 'mobile-glass-lens', width: Math.round(width), height, borderRadius: radius,
      scale: -35, aberration: [0, 0.35, 0.7], border: 0.06, blur: 2, alpha: 0.99,
      saturation: 1.05, autoResize: false, fallbackFilter: 'blur(0.6px) saturate(1.05)'
    };
    try {
      if (lens) lens.update(options); else lens = api.createLiquidGlass(capsule, options);
      lensKey = key; classState(nav, 'liquid-glass-active', lens.isActive);
      nav.dataset.glass = lens.isActive ? 'refraction' : 'blur';
    } catch (_) { destroyLens(); nav.dataset.glass = 'blur'; }
  }
  function refresh() { if (!refreshFrame) refreshFrame = requestAnimationFrame(refreshNow); }
  function layoutChanged() { cancelGesture(true, true); refresh(); }
  function pointerButton(event) {
    const button = event.target instanceof Element ? event.target.closest('button[data-page]') : null;
    return buttons.includes(button) ? button : null;
  }
  nav.addEventListener('pointerdown', event => {
    if (!visible() || !event.isPrimary || (event.pointerType === 'mouse' && event.button !== 0)) return;
    const button = pointerButton(event);
    if (!button || button.disabled || pointer) return;
    suppressClick = null;
    const rect = nav.getBoundingClientRect();
    pointer = { id: event.pointerId, startX: event.clientX, startY: event.clientY,
      lastX: event.clientX, lastTime: performance.now(), rect, button, dragging: false,
      offset: event.clientX - rect.left - (button.offsetLeft + button.offsetWidth / 2) };
    classState(nav, 'is-settling', false); classState(nav, 'is-pressed', true); preview(button);
    moveTo({ ...buttonPoint(button), sx: noMotion() ? 1 : 1.10, sy: noMotion() ? 1 : 1.12, gx: 44, gy: 16 });
  });
  document.addEventListener('pointerdown', event => {
    if (pointer && event.pointerId !== pointer.id) cancelGesture(false, true);
  }, { capture: true, passive: true });
  document.addEventListener('pointermove', event => {
    if (!pointer || event.pointerId !== pointer.id) return;
    const dx = event.clientX - pointer.startX, dy = event.clientY - pointer.startY;
    if (!pointer.dragging) {
      if (Math.abs(dy) > 6 && Math.abs(dy) > Math.abs(dx)) { cancelGesture(false, true); return; }
      if (Math.abs(dx) <= 5 || Math.abs(dx) <= Math.abs(dy)) return;
      pointer.dragging = true; classState(nav, 'is-dragging', true);
      try { nav.setPointerCapture(pointer.id); } catch (_) { /* Document handlers still finish the gesture. */ }
    }
    if (event.cancelable) event.preventDefault();
    const now = performance.now();
    const speed = clamp((event.clientX - pointer.lastX) / Math.max(now - pointer.lastTime, 8), -2, 2);
    pointer.lastX = event.clientX; pointer.lastTime = now;
    const localX = event.clientX - pointer.rect.left - pointer.offset;
    const minCenter = buttons[0].offsetLeft + buttons[0].offsetWidth / 2;
    const last = buttons[buttons.length - 1], maxCenter = last.offsetLeft + last.offsetWidth / 2;
    const center = clamp(localX, minCenter, maxCenter), nearest = closestButton(center);
    preview(nearest);
    const reduced = noMotion();
    moveTo({ x: center - width / 2, y: buttonPoint(nearest).y,
      sx: reduced ? 1 : 1.10 + Math.min(Math.abs(speed) * 0.055, 0.11),
      sy: reduced ? 1 : 1.12 - Math.min(Math.abs(speed) * 0.038, 0.07),
      gx: clamp(42 + speed * 13, 18, 78), gy: clamp(13 + (event.clientY - pointer.startY) * 0.3, 5, 26) });
  }, { passive: false });
  document.addEventListener('pointerup', event => {
    if (!pointer || event.pointerId !== pointer.id) return;
    const previous = pointer; pointer = null; releaseCapture(previous.id);
    classState(nav, 'is-pressed', false); classState(nav, 'is-dragging', false); preview(null);
    if (previous.dragging) {
      blockCompatibilityClick(previous.id);
      const nearest = closestButton(event.clientX - previous.rect.left - previous.offset);
      // Keep existing page navigation as the only commit path: no API actions.
      if (!nearest.disabled) nearest.click();
      syncNavigation(); restAtSelection(false);
      if (event.cancelable) event.preventDefault();
    } else restAtSelection(false); // Ordinary taps retain their browser click.
  }, { passive: false });
  document.addEventListener('pointercancel', event => {
    if (pointer && event.pointerId === pointer.id) cancelGesture(false, true);
  }, { passive: true });
  nav.addEventListener('lostpointercapture', event => {
    // Touch begins with implicit capture on a child button. Transferring it
    // to this dock emits a bubbled loss from that child, not a cancelled drag.
    if (event.target === nav && pointer && event.pointerId === pointer.id) cancelGesture(false, true);
  });
  nav.addEventListener('click', event => {
    if (!suppressClick || event.detail === 0) return;
    if (performance.now() > suppressClick.until) { suppressClick = null; return; }
    if (typeof event.pointerId === 'number' && event.pointerId > 0 && event.pointerId !== suppressClick.id) return;
    suppressClick = null; event.preventDefault(); event.stopImmediatePropagation();
  }, { capture: true });
  const appObserver = new MutationObserver(() => { if (!visible()) cancelGesture(true, true); refresh(); });
  appObserver.observe(app, { attributes: true, attributeFilter: ['class'] });
  const themeObserver = new MutationObserver(layoutChanged);
  themeObserver.observe(root, { attributes: true, attributeFilter: ['data-theme', 'class'] });
  const navigationObserver = new MutationObserver(syncNavigation);
  buttons.forEach(button => navigationObserver.observe(button, { attributes: true, attributeFilter: ['class'] }));
  if (typeof ResizeObserver === 'function') new ResizeObserver(layoutChanged).observe(nav);
  window.addEventListener('resize', layoutChanged, { passive: true });
  document.addEventListener('visibilitychange', () => { cancelGesture(true, true); refresh(); });
  window.addEventListener('pageshow', layoutChanged);
  for (const query of Object.values(media)) {
    if (typeof query.addEventListener === 'function') query.addEventListener('change', layoutChanged);
    else if (typeof query.addListener === 'function') query.addListener(layoutChanged);
  }
  if (navigator.connection && typeof navigator.connection.addEventListener === 'function') {
    navigator.connection.addEventListener('change', layoutChanged);
  }
  window.GlassUI = Object.freeze({ syncNavigation, refresh });
  refreshNow();
})();
