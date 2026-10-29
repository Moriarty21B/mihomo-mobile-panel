/*
MIT License

Copyright (c) 2026 Riz Roze

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

Local adaptation: classic script, safe lifecycle and bounded map cache.
*/
(function () {
'use strict';
/**
 * Liquid Glass — Real optical refraction for the web.
 *
 * Uses Canvas 2D to generate displacement maps fed into SVG feDisplacementMap
 * filters. Three separate passes (one per RGB channel) with slightly different
 * scale values create chromatic aberration at the edges.
 *
 * Chromium only. Safari/Firefox fall back to regular backdrop-filter: blur().
 *
 * @see https://github.com/rizzytoday/liquid-glass
 * @license MIT
 */
function resolveConfig(el, opts) {
  const rect = el.getBoundingClientRect();
  const ab = opts.aberration ?? [0, 10, 20];
  const width = Math.round(opts.width ?? rect.width);
  const height = Math.round(opts.height ?? rect.height);
  if (!Number.isFinite(width) || !Number.isFinite(height) || width < 1 || height < 1 || width > 4096 || height > 4096) {
    throw new Error("Liquid Glass requires a visible element of a supported size");
  }
  return {
    width,
    height,
    radius: Math.min(opts.borderRadius ?? 50, width / 2, height / 2),
    scale: opts.scale ?? -180,
    border: opts.border ?? 0.07,
    lightness: opts.lightness ?? 50,
    alpha: opts.alpha ?? 0.93,
    blur: opts.blur ?? 11,
    r: ab[0],
    g: ab[1],
    b: ab[2],
    frost: opts.frost ?? 0,
    saturation: opts.saturation ?? 1,
    displace: opts.displaceBlur ?? 0
  };
}
const isChromium = typeof navigator !== "undefined" && /Chrome\//.test(navigator.userAgent);
const _mapCache = /* @__PURE__ */ new Map();
function buildDisplacementMap(c) {
  const key = `${c.width}:${c.height}:${c.radius}:${c.scale}:${c.border}:${c.blur}:${c.lightness}:${c.alpha}`;
  const cached = _mapCache.get(key);
  if (cached) return cached;
  const maxDisplace = Math.max(Math.abs(c.scale) * 0.5, 20);
  const padX = Math.ceil(maxDisplace);
  const padY = Math.ceil(maxDisplace);
  const totalW = c.width + padX * 2;
  const totalH = c.height + padY * 2;
  const canvas = document.createElement("canvas");
  canvas.width = totalW;
  canvas.height = totalH;
  const ctx = canvas.getContext("2d");
  if (!ctx || typeof ctx.roundRect !== "function") throw new Error("Canvas rounding is unavailable");
  ctx.fillStyle = "rgb(128, 128, 128)";
  ctx.fillRect(0, 0, totalW, totalH);
  const ox = padX;
  const oy = padY;
  ctx.save();
  ctx.beginPath();
  ctx.roundRect(ox, oy, c.width, c.height, c.radius);
  ctx.clip();
  ctx.fillStyle = "#000000";
  ctx.fillRect(ox, oy, c.width, c.height);
  const redGrad = ctx.createLinearGradient(ox + c.width, oy, ox, oy);
  redGrad.addColorStop(0, "#000000");
  redGrad.addColorStop(1, "#ff0000");
  ctx.fillStyle = redGrad;
  ctx.fillRect(ox, oy, c.width, c.height);
  ctx.globalCompositeOperation = "difference";
  const blueGrad = ctx.createLinearGradient(ox, oy, ox, oy + c.height);
  blueGrad.addColorStop(0, "#000000");
  blueGrad.addColorStop(1, "#0000ff");
  ctx.fillStyle = blueGrad;
  ctx.fillRect(ox, oy, c.width, c.height);
  ctx.globalCompositeOperation = "source-over";
  const borderPx = Math.min(c.width, c.height) * (c.border * 0.5);
  ctx.filter = `blur(${c.blur}px)`;
  ctx.fillStyle = `hsla(0, 0%, ${c.lightness}%, ${c.alpha})`;
  ctx.beginPath();
  ctx.roundRect(
    ox + borderPx,
    oy + borderPx,
    c.width - borderPx * 2,
    c.height - borderPx * 2,
    c.radius
  );
  ctx.fill();
  ctx.restore();
  const uri = canvas.toDataURL();
  // A rotated phone may produce several sizes; keep only a small recent set.
  if (_mapCache.size >= 8) _mapCache.delete(_mapCache.keys().next().value);
  _mapCache.set(key, uri);
  return uri;
}
let _instanceCount = 0;
function createFilterSVG(id) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("xmlns", "http://www.w3.org/2000/svg");
  svg.style.cssText = "position:absolute;width:0;height:0;pointer-events:none;";
  svg.innerHTML = `
    <defs>
      <filter id="${id}" color-interpolation-filters="sRGB" x="-38%" y="-188%" width="176%" height="476%">
        <feImage result="map" preserveAspectRatio="none" />
        <feDisplacementMap in="SourceGraphic" in2="map" xChannelSelector="R" yChannelSelector="B" result="dispRed" data-channel="red" />
        <feColorMatrix in="dispRed" type="matrix" values="1 0 0 0 0  0 0 0 0 0  0 0 0 0 0  0 0 0 1 0" result="red" />
        <feDisplacementMap in="SourceGraphic" in2="map" xChannelSelector="R" yChannelSelector="B" result="dispGreen" data-channel="green" />
        <feColorMatrix in="dispGreen" type="matrix" values="0 0 0 0 0  0 1 0 0 0  0 0 0 0 0  0 0 0 1 0" result="green" />
        <feDisplacementMap in="SourceGraphic" in2="map" xChannelSelector="R" yChannelSelector="B" result="dispBlue" data-channel="blue" />
        <feColorMatrix in="dispBlue" type="matrix" values="0 0 0 0 0  0 0 0 0 0  0 0 1 0 0  0 0 0 1 0" result="blue" />
        <feBlend in="red" in2="green" mode="screen" result="rg" />
        <feBlend in="rg" in2="blue" mode="screen" result="output" />
        <feGaussianBlur in="output" stdDeviation="0" />
      </filter>
    </defs>
  `;
  const filter = svg.querySelector("filter");
  const feImage = svg.querySelector("feImage");
  const red = svg.querySelector('[data-channel="red"]');
  const green = svg.querySelector('[data-channel="green"]');
  const blue = svg.querySelector('[data-channel="blue"]');
  const blurEl = svg.querySelector("feGaussianBlur");
  return { svg, feImage, red, green, blue, blur: blurEl, filter };
}
function applyConfig(c, refs) {
  const uri = buildDisplacementMap(c);
  const maxD = Math.max(Math.abs(c.scale) * 0.5, 20);
  const pctX = Math.ceil(maxD / c.width * 100);
  const pctY = Math.ceil(maxD / c.height * 100);
  refs.filter.setAttribute("x", `-${pctX}%`);
  refs.filter.setAttribute("y", `-${pctY}%`);
  refs.filter.setAttribute("width", `${100 + pctX * 2}%`);
  refs.filter.setAttribute("height", `${100 + pctY * 2}%`);
  refs.feImage.setAttributeNS("http://www.w3.org/1999/xlink", "href", uri);
  refs.feImage.setAttribute("href", uri);
  refs.red.setAttribute("scale", String(c.scale + c.r));
  refs.green.setAttribute("scale", String(c.scale + c.g));
  refs.blue.setAttribute("scale", String(c.scale + c.b));
  refs.blur.setAttribute("stdDeviation", String(c.displace));
}
function createLiquidGlass(element, options = {}) {
  const fallback = options.fallbackFilter ?? "blur(12px)";
  const properties = ["backdrop-filter", "-webkit-backdrop-filter", "background"];
  const previous = properties.map(name => [name, element.style.getPropertyValue(name), element.style.getPropertyPriority(name)]);
  const restoreStyles = () => {
    for (const [name, value, priority] of previous) {
      if (value) element.style.setProperty(name, value, priority);
      else element.style.removeProperty(name);
    }
  };
  const canvasSupported = typeof CanvasRenderingContext2D !== "undefined" && typeof CanvasRenderingContext2D.prototype.roundRect === "function";
  if (!isChromium || !canvasSupported) {
    element.style.backdropFilter = fallback;
    element.style.setProperty("-webkit-backdrop-filter", fallback);
    const dummySvg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    return {
      isActive: false,
      filterElement: dummySvg,
      update() {
      },
      destroy() {
        restoreStyles();
      }
    };
  }
  let currentOpts = { ...options };
  let config = resolveConfig(element, currentOpts);
  const requestedId = options.filterId;
  const id = requestedId && /^[A-Za-z][\w-]*$/.test(requestedId) ? requestedId : `liquid-glass-${++_instanceCount}`;
  const refs = createFilterSVG(id);
  try {
    applyConfig(config, refs);
    document.body.appendChild(refs.svg);
  } catch (error) {
    refs.svg.remove();
    restoreStyles();
    throw error;
  }
  let destroyed = false;
  const applyStyles = (c) => {
    element.style.backdropFilter = `url(#${id}) saturate(${c.saturation})`;
    element.style.setProperty(
      "-webkit-backdrop-filter",
      `url(#${id}) saturate(${c.saturation})`
    );
    if (c.frost > 0) {
      element.style.background = `hsl(0 0% 0% / ${c.frost})`;
    }
  };
  applyStyles(config);
  let resizeRaf = 0;
  const ro = options.autoResize !== false && typeof ResizeObserver === "function" ? new ResizeObserver(() => {
    cancelAnimationFrame(resizeRaf);
    resizeRaf = requestAnimationFrame(() => {
      const rect = element.getBoundingClientRect();
      if (!destroyed && rect.width > 0 && rect.height > 0 && (currentOpts.width == null || currentOpts.height == null)) {
        try {
          config = resolveConfig(element, currentOpts);
          applyConfig(config, refs);
          applyStyles(config);
        } catch (_) {
          // Preserve the last working lens until the next visible resize.
        }
      }
    });
  }) : null;
  if (ro) ro.observe(element);
  return {
    isActive: true,
    filterElement: refs.svg,
    update(newOpts) {
      if (destroyed) return;
      currentOpts = { ...currentOpts, ...newOpts };
      config = resolveConfig(element, currentOpts);
      applyConfig(config, refs);
      applyStyles(config);
    },
    destroy() {
      if (destroyed) return;
      destroyed = true;
      if (ro) ro.disconnect();
      cancelAnimationFrame(resizeRaf);
      refs.svg.remove();
      restoreStyles();
    }
  };
}
window.LiquidGlass = Object.freeze({ createLiquidGlass, isChromium });
})();
