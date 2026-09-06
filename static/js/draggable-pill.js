// 通用可拖动 pill（apple-design：§2 直接操控 / §5 速度交接 / §6 动量投影 / §9 边界）
// 松手时按释放速度 + 当前位置投影到最近的屏幕边缘贴边（PiP 式吸附），
// 并用一次短促的平滑过渡滑入落点，而不是硬停。

function makeDraggablePill(el, storageKey) {
    let dragging = false, startX, startY, origX, origY, moved = false;
    // 最近 5 帧指针轨迹（位置 + 时间戳），用于释放时估算速度
    let history = [];
    const HISTORY_MAX = 5;
    const EDGE_SNAP_PX = 90;     // 距边缘多少以内松手则贴边
    const FLING_EDGE_PX = 26;    // 释放速度达到多少 px/帧（≈520px/s）视为甩动

    function clamp(x, y) {
        const w = el.offsetWidth, h = el.offsetHeight;
        const maxX = window.innerWidth - w - 8;
        const maxY = window.innerHeight - h - 8;
        return { x: Math.max(8, Math.min(x, maxX)), y: Math.max(8, Math.min(y, maxY)) };
    }
    function applyPos(x, y) {
        const p = clamp(x, y);
        el.style.left = p.x + 'px';
        el.style.top = p.y + 'px';
        el.style.right = 'auto';
        el.style.bottom = 'auto';
        return p;
    }
    function loadPos() {
        try {
            const raw = localStorage.getItem(storageKey);
            if (raw) { const { x, y } = JSON.parse(raw); applyPos(x, y); }
        } catch (e) {}
    }
    function savePos() {
        const rect = el.getBoundingClientRect();
        try { localStorage.setItem(storageKey, JSON.stringify({ x: rect.left, y: rect.top })); } catch (e) {}
    }

    el.style.position = 'fixed';
    el.style.touchAction = 'none';
    el.style.cursor = 'grab';

    el.addEventListener('pointerdown', function(e) {
        if (e.target.closest('button')) return; // 点按钮不触发拖拽
        dragging = true; moved = false; history = [];
        const rect = el.getBoundingClientRect();
        startX = e.clientX; startY = e.clientY;
        origX = rect.left; origY = rect.top;
        el.setPointerCapture(e.pointerId);
        el.classList.add('dragging');
        // 拖拽期间关闭平滑过渡，保证 1:1 跟手
        el.style.transition = 'none';
    });
    el.addEventListener('pointermove', function(e) {
        if (!dragging) return;
        const dx = e.clientX - startX, dy = e.clientY - startY;
        if (Math.abs(dx) > 3 || Math.abs(dy) > 3) moved = true;
        applyPos(origX + dx, origY + dy);
        history.push({ x: e.clientX, y: e.clientY, t: performance.now() });
        if (history.length > HISTORY_MAX) history.shift();
    });
    function endDrag(e) {
        if (!dragging) return;
        dragging = false;
        el.classList.remove('dragging');
        if (!moved) { el.style.transition = ''; return; }

        // 估算释放速度（最近两帧位移 / 帧间隔），再投影落点：贴着边缘停
        let vx = 0;
        if (history.length >= 2) {
            const a = history[history.length - 2], b = history[history.length - 1];
            const dt = Math.max(1, b.t - a.t);
            vx = (b.x - a.x) / dt; // px/ms
        }
        const rect = el.getBoundingClientRect();
        const viewW = window.innerWidth;
        const leftDist = rect.left - 8, rightDist = viewW - rect.right - 8;
        let targetX = rect.left;
        // 甩动：速度方向指向的那一侧直接贴边
        if (vx < -FLING_EDGE_PX / 16.67) targetX = 8;
        else if (vx > FLING_EDGE_PX / 16.67) targetX = viewW - rect.width - 8;
        // 静止松手但已接近某侧边缘：吸附过去
        else if (leftDist < EDGE_SNAP_PX && leftDist <= rightDist) targetX = 8;
        else if (rightDist < EDGE_SNAP_PX && rightDist < leftDist) targetX = viewW - rect.width - 8;

        // 短促平滑滑入落点（响应 ~0.25s，临界阻尼无回弹——PiP 贴边不该弹）
        el.style.transition = 'left 0.25s cubic-bezier(0.25, 0.1, 0.25, 1), top 0.25s cubic-bezier(0.25, 0.1, 0.25, 1)';
        applyPos(targetX, rect.top);
        setTimeout(function() { el.style.transition = ''; }, 300);
        savePos();
    }
    el.addEventListener('pointerup', endDrag);
    el.addEventListener('pointercancel', endDrag);
    window.addEventListener('resize', loadPos);
    loadPos();

    return { wasMoved: () => moved };
}
