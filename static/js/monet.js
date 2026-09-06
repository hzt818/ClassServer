// monet.js — 全局"莫奈取色"。
(function () {
    'use strict';

    const SOURCE_IMAGE = '/static/images/login_img.jpg';
    const CACHE_KEY = 'monetPalette:v2:' + SOURCE_IMAGE;
    const SAMPLE_SIZE = 48;

    function rgbToHsl(r, g, b) {
        r /= 255; g /= 255; b /= 255;
        const max = Math.max(r, g, b), min = Math.min(r, g, b);
        let h, s, l = (max + min) / 2;
        if (max === min) { h = s = 0; }
        else {
            const d = max - min;
            s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
            switch (max) {
                case r: h = (g - b) / d + (g < b ? 6 : 0); break;
                case g: h = (b - r) / d + 2; break;
                default: h = (r - g) / d + 4;
            }
            h *= 60;
        }
        return { h, s, l };
    }

    function hslToCss(h, s, l, a) {
        h = ((h % 360) + 360) % 360;
        s = Math.max(0, Math.min(1, s)) * 100;
        l = Math.max(0, Math.min(1, l)) * 100;
        if (a === undefined) return `hsl(${h.toFixed(1)}, ${s.toFixed(1)}%, ${l.toFixed(1)}%)`;
        return `hsla(${h.toFixed(1)}, ${s.toFixed(1)}%, ${l.toFixed(1)}%, ${a})`;
    }

    function extractSeedHsl(img) {
        const canvas = document.createElement('canvas');
        canvas.width = SAMPLE_SIZE;
        canvas.height = SAMPLE_SIZE;
        const ctx = canvas.getContext('2d', { willReadFrequently: true });
        ctx.drawImage(img, 0, 0, SAMPLE_SIZE, SAMPLE_SIZE);

        let data;
        try {
            data = ctx.getImageData(0, 0, SAMPLE_SIZE, SAMPLE_SIZE).data;
        } catch (e) {
            // 常见于图片跨域/被判定为"被污染的画布"时抛 SecurityError，这种情况下没法读像素，直接走兜底色调。
            console.warn('[莫奈取色] 无法读取图片像素（可能是跨域限制）:', e);
            return { h: 245, s: 0.7, l: 0.66 };
        }

        const BUCKET = 24;
        const buckets = new Map();

        for (let i = 0; i < data.length; i += 4) {
            const r = data[i], g = data[i + 1], b = data[i + 2], a = data[i + 3];
            if (a < 128) continue;
            const key = [
                Math.round(r / BUCKET),
                Math.round(g / BUCKET),
                Math.round(b / BUCKET),
            ].join(',');
            const hsl = rgbToHsl(r, g, b);
            const lightnessPenalty = 1 - Math.abs(hsl.l - 0.5) * 1.4;
            const weight = Math.max(0.05, hsl.s) * Math.max(0.15, lightnessPenalty);

            let entry = buckets.get(key);
            if (!entry) {
                entry = { weight: 0, sumR: 0, sumG: 0, sumB: 0, n: 0 };
                buckets.set(key, entry);
            }
            entry.weight += weight;
            entry.sumR += r; entry.sumG += g; entry.sumB += b; entry.n += 1;
        }

        let best = null;
        for (const entry of buckets.values()) {
            if (!best || entry.weight > best.weight) best = entry;
        }
        if (!best) return { h: 245, s: 0.7, l: 0.66 };

        return rgbToHsl(best.sumR / best.n, best.sumG / best.n, best.sumB / best.n);
    }

    function buildPaletteVars(seed) {
        const h = seed.h;
        const satBoost = Math.max(0.55, Math.min(0.85, seed.s + 0.15));

        return {
            '--accent': hslToCss(h, satBoost, 0.66),
            '--accent-2': hslToCss(h + 8, satBoost, 0.76),
            '--accent-soft': hslToCss(h, satBoost, 0.66, 0.15),
            '--accent-soft-strong': hslToCss(h, satBoost, 0.66, 0.35),
            '--bg-deep': hslToCss(h, 0.18, 0.06),
            '--glass-bg': hslToCss(h, 0.25, 0.92, 0.06),
            '--glass-bg-strong': hslToCss(h, 0.25, 0.92, 0.10),
            '--glass-border': hslToCss(h, 0.30, 0.90, 0.16),
            '--glass-highlight': hslToCss(h, 0.20, 0.98, 0.35),
            '--monet-glow-1': hslToCss(h, 0.65, 0.55, 0.20),
            '--monet-glow-2': hslToCss(h + 40, 0.55, 0.55, 0.14),
        };
    }

    function applyPalette(vars) {
        const root = document.documentElement.style;
        for (const key in vars) root.setProperty(key, vars[key]);
    }

    // 完整的换色实现。
    function injectMonetStylesheet() {
        const css = `
            body {
                background:
                    radial-gradient(1200px 800px at 15% -10%, var(--monet-glow-1), transparent 60%),
                    radial-gradient(1000px 700px at 110% 10%, var(--monet-glow-2), transparent 55%),
                    var(--bg-deep, #121212) !important;
                background-attachment: fixed !important;
            }
            button[type="submit"], .custom-btn, .submit-btn {
                background: linear-gradient(135deg, var(--accent), var(--accent-2)) !important;
                box-shadow: 0 6px 20px var(--accent-soft), inset 0 1px 0 rgba(255,255,255,0.35) !important;
            }
            .input-group input:focus {
                border-color: var(--accent-2) !important;
                box-shadow: 0 0 0 4px var(--accent-soft) !important;
            }
            .lan-ip-value { color: var(--accent-2) !important; }
            .lan-ip-banner {
                background: var(--glass-bg-strong) !important;
                border-color: var(--accent-soft-strong) !important;
            }
            .about-footer .highlight { color: var(--accent) !important; }
            .sidebar li.active a {
                background: linear-gradient(135deg, var(--accent-soft-strong), var(--accent-soft)) !important;
                border-color: var(--accent-soft-strong) !important;
                box-shadow: 0 4px 16px var(--accent-soft), inset 0 1px 0 rgba(255,255,255,0.25) !important;
            }
            .role-badge {
                background: var(--accent-soft) !important;
                border-color: var(--accent-soft-strong) !important;
                color: var(--accent-2) !important;
            }
            .feature-card i { color: var(--accent-2) !important; }
            .feature-card:hover {
                box-shadow: 0 16px 40px rgba(0,0,0,0.5), inset 0 1px 0 var(--accent-soft) !important;
            }
            #globalMentionBanner, #voiceToastBanner, .mention-banner {
                background: linear-gradient(135deg, var(--accent-soft-strong), var(--accent-soft)) !important;
            }
        `;
        let styleEl = document.getElementById('monet-dynamic-style');
        if (!styleEl) {
            styleEl = document.createElement('style');
            styleEl.id = 'monet-dynamic-style';
            document.head.appendChild(styleEl);
        }
        styleEl.textContent = css;
    }

    try {
        const cached = localStorage.getItem(CACHE_KEY);
        if (cached) {
            applyPalette(JSON.parse(cached));
            injectMonetStylesheet();
        }
    } catch (e) { /* 忽略缓存读取失败，走默认配色 */ }

    const img = new Image();
    img.crossOrigin = 'anonymous'; // 同源资源不受影响，防止极端跨域画布污染
    img.onload = function () {
        try {
            const seed = extractSeedHsl(img);
            const vars = buildPaletteVars(seed);
            applyPalette(vars);
            injectMonetStylesheet();
            localStorage.setItem(CACHE_KEY, JSON.stringify(vars));
        } catch (e) {
            console.warn('[莫奈取色] 提取失败，使用默认配色:', e);
        }
    };
    img.onerror = function () {
        console.warn('[莫奈取色] 基准图片加载失败，使用默认配色');
    };
    img.src = SOURCE_IMAGE;
})();