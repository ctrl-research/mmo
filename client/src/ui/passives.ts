import type { PassiveState } from "@/net/connection";

/**
 * The passive tree screen.
 *
 * Canvas rather than DOM. Several hundred nodes that pan, zoom, and highlight
 * on hover is the case a canvas is for; the same thing in elements is hundreds
 * of layout boxes recalculated on every drag.
 *
 * The tree itself comes over HTTP once, because it is content and identical
 * for everybody. What arrives on the socket is which nodes this character
 * holds -- resending several hundred positions every time somebody spends a
 * point would be resending the same thing over and over.
 */

export interface TreeNode {
  id: number;
  kind: "start" | "small" | "notable" | "keystone";
  name?: string;
  x: number;
  y: number;
  lines: string[];
}

export interface Tree {
  nodes: TreeNode[];
  edges: [number, number][];
  classStarts: Record<string, number>;
}

export interface PassiveCallbacks {
  onAllocate(nodeId: number): void;
  onRefund(nodeId: number): void;
  onRespec(): void;
}

/** How big each kind of node is drawn, in tree units. */
const RADIUS: Record<TreeNode["kind"], number> = {
  start: 22,
  small: 9,
  notable: 15,
  keystone: 20,
};

const COLOURS = {
  edge: "#2a3140",
  edgeHeld: "#c8a24a",

  // Three states, three colours: held, reachable, and out of reach. A player
  // scanning the tree needs to see where they can go without reading anything.
  held: "#e0c04a",
  available: "#6fa8ff",
  locked: "#39435a",

  text: "#e6e8ee",
  dim: "#6b7280",
};

export class PassivePanel {
  #root: HTMLElement;
  #canvas: HTMLCanvasElement;
  #ctx: CanvasRenderingContext2D;
  #cb: PassiveCallbacks;

  #tree: Tree | null = null;
  #state: PassiveState | null = null;

  #byId = new Map<number, TreeNode>();
  #adjacency = new Map<number, number[]>();

  #open = false;

  // The view: where the tree is under the window, and how far in.
  #panX = 0;
  #panY = 0;
  #zoom = 1;

  #dragging = false;
  #dragMoved = false;
  #lastX = 0;
  #lastY = 0;
  #hover: TreeNode | null = null;

  constructor(root: HTMLElement, cb: PassiveCallbacks) {
    this.#root = root;
    this.#cb = cb;

    root.innerHTML = `
      <div class="tree-header">
        <span>Passive tree</span>
        <span class="tree-points"></span>
        <button class="tree-respec">Respec all</button>
        <span class="tree-hint">drag to pan &middot; scroll to zoom &middot; P to close</span>
      </div>
      <canvas class="tree-canvas"></canvas>
      <div class="tree-tooltip" hidden></div>`;

    this.#canvas = root.querySelector("canvas")!;
    this.#ctx = this.#canvas.getContext("2d")!;

    root.querySelector(".tree-respec")!.addEventListener("click", () => this.#cb.onRespec());

    this.#canvas.addEventListener("pointerdown", (e) => this.#onDown(e));
    this.#canvas.addEventListener("pointermove", (e) => this.#onMove(e));
    this.#canvas.addEventListener("pointerup", (e) => this.#onUp(e));
    this.#canvas.addEventListener("pointerleave", () => {
      this.#dragging = false;
      this.#hover = null;
      this.#drawTooltip();
    });
    this.#canvas.addEventListener("wheel", (e) => this.#onWheel(e), { passive: false });
  }

  get isOpen(): boolean {
    return this.#open;
  }

  async toggle(): Promise<void> {
    this.#open = !this.#open;
    this.#root.hidden = !this.#open;
    if (!this.#open) return;

    if (!this.#tree) {
      await this.#load();
      this.#centreOnStart();
    }
    this.#resize();
    this.render();
  }

  close(): void {
    this.#open = false;
    this.#root.hidden = true;
  }

  update(state: PassiveState): void {
    this.#state = state;
    if (this.#open) this.render();
  }

  async #load(): Promise<void> {
    const res = await fetch("/api/passives");
    if (!res.ok) throw new Error(`could not load the passive tree: ${res.status}`);

    this.#tree = await res.json();

    for (const node of this.#tree!.nodes) this.#byId.set(node.id, node);
    for (const [a, b] of this.#tree!.edges) {
      if (!this.#adjacency.has(a)) this.#adjacency.set(a, []);
      if (!this.#adjacency.has(b)) this.#adjacency.set(b, []);
      this.#adjacency.get(a)!.push(b);
      this.#adjacency.get(b)!.push(a);
    }
  }

  /** Opens the view on the character's own start, not the tree's origin. */
  #centreOnStart(): void {
    const start = this.#byId.get(this.#state?.startNode ?? 0);
    if (!start) return;

    this.#panX = -start.x;
    this.#panY = -start.y;
  }

  #resize(): void {
    const dpr = window.devicePixelRatio || 1;
    const rect = this.#canvas.getBoundingClientRect();

    this.#canvas.width = Math.round(rect.width * dpr);
    this.#canvas.height = Math.round(rect.height * dpr);
    this.#ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }

  /** Whether a node is held, reachable, or out of reach. */
  #status(id: number): "held" | "available" | "locked" {
    const state = this.#state;
    if (!state) return "locked";

    if (id === state.startNode || state.allocated.includes(id)) return "held";

    for (const next of this.#adjacency.get(id) ?? []) {
      if (next === state.startNode || state.allocated.includes(next)) return "available";
    }
    return "locked";
  }

  render(): void {
    if (!this.#open || !this.#tree) return;

    const ctx = this.#ctx;
    const rect = this.#canvas.getBoundingClientRect();

    ctx.clearRect(0, 0, rect.width, rect.height);

    const cx = rect.width / 2;
    const cy = rect.height / 2;
    const toScreen = (x: number, y: number): [number, number] => [
      cx + (x + this.#panX) * this.#zoom,
      cy + (y + this.#panY) * this.#zoom,
    ];

    // Edges first, so nodes sit on top of them. A held edge is drawn brighter,
    // which is what makes a route through the tree readable at a glance.
    for (const [a, b] of this.#tree.edges) {
      const from = this.#byId.get(a);
      const to = this.#byId.get(b);
      if (!from || !to) continue;

      const bothHeld = this.#status(a) === "held" && this.#status(b) === "held";
      ctx.strokeStyle = bothHeld ? COLOURS.edgeHeld : COLOURS.edge;
      ctx.lineWidth = bothHeld ? 3 : 1.5;

      ctx.beginPath();
      ctx.moveTo(...toScreen(from.x, from.y));
      ctx.lineTo(...toScreen(to.x, to.y));
      ctx.stroke();
    }

    for (const node of this.#tree.nodes) {
      const [x, y] = toScreen(node.x, node.y);
      const r = RADIUS[node.kind] * this.#zoom;

      // Off screen: skip rather than draw. At full zoom-out most of the tree
      // is on screen anyway, and at full zoom-in most of it is not.
      if (x < -r || y < -r || x > rect.width + r || y > rect.height + r) continue;

      const status = this.#status(node.id);
      ctx.fillStyle = COLOURS[status];
      ctx.globalAlpha = status === "locked" ? 0.5 : 1;

      ctx.beginPath();
      ctx.arc(x, y, r, 0, Math.PI * 2);
      ctx.fill();

      // Keystones and notables get a ring, so the shape of the tree reads
      // before any of the text does.
      if (node.kind === "keystone" || node.kind === "start") {
        ctx.strokeStyle = COLOURS.text;
        ctx.lineWidth = 2;
        ctx.stroke();
      }
      ctx.globalAlpha = 1;

      // Names only when they will fit and be legible.
      if (node.name && this.#zoom > 0.55) {
        ctx.fillStyle = status === "locked" ? COLOURS.dim : COLOURS.text;
        ctx.font = "11px ui-monospace, Menlo, monospace";
        ctx.textAlign = "center";
        ctx.fillText(node.name, x, y + r + 13);
      }
    }

    const points = this.#root.querySelector(".tree-points")!;
    points.textContent = this.#state
      ? `${this.#state.spentPoints} of ${this.#state.totalPoints} points spent`
      : "";
  }

  // --- interaction -----------------------------------------------------------

  #nodeAt(clientX: number, clientY: number): TreeNode | null {
    if (!this.#tree) return null;

    const rect = this.#canvas.getBoundingClientRect();
    const px = clientX - rect.left;
    const py = clientY - rect.top;

    const cx = rect.width / 2;
    const cy = rect.height / 2;

    let best: TreeNode | null = null;
    let bestDist = Infinity;

    for (const node of this.#tree.nodes) {
      const x = cx + (node.x + this.#panX) * this.#zoom;
      const y = cy + (node.y + this.#panY) * this.#zoom;
      // A generous radius: these are small targets, and a click that lands
      // near a node meant that node.
      const r = Math.max(RADIUS[node.kind] * this.#zoom, 10);

      const dist = Math.hypot(px - x, py - y);
      if (dist <= r && dist < bestDist) {
        best = node;
        bestDist = dist;
      }
    }
    return best;
  }

  #onDown(e: PointerEvent): void {
    this.#dragging = true;
    this.#dragMoved = false;
    this.#lastX = e.clientX;
    this.#lastY = e.clientY;
    this.#canvas.setPointerCapture(e.pointerId);
  }

  #onMove(e: PointerEvent): void {
    if (this.#dragging) {
      const dx = e.clientX - this.#lastX;
      const dy = e.clientY - this.#lastY;

      // A few pixels of slop, so a click with a shaky hand is still a click
      // rather than a one-pixel drag that swallows it.
      if (Math.abs(dx) > 2 || Math.abs(dy) > 2) this.#dragMoved = true;

      this.#panX += dx / this.#zoom;
      this.#panY += dy / this.#zoom;
      this.#lastX = e.clientX;
      this.#lastY = e.clientY;

      this.render();
      return;
    }

    const under = this.#nodeAt(e.clientX, e.clientY);
    if (under !== this.#hover) {
      this.#hover = under;
      this.#drawTooltip(e.clientX, e.clientY);
    } else if (under) {
      this.#drawTooltip(e.clientX, e.clientY);
    }
  }

  #onUp(e: PointerEvent): void {
    this.#dragging = false;
    this.#canvas.releasePointerCapture(e.pointerId);

    if (this.#dragMoved) return;

    const node = this.#nodeAt(e.clientX, e.clientY);
    if (!node) return;

    // Clicking a held node refunds it and clicking a reachable one takes it.
    // One control for both, because they are the same decision in two
    // directions.
    const status = this.#status(node.id);
    if (status === "held") {
      this.#cb.onRefund(node.id);
    } else if (status === "available") {
      this.#cb.onAllocate(node.id);
    }
  }

  #onWheel(e: WheelEvent): void {
    e.preventDefault();

    const factor = e.deltaY < 0 ? 1.15 : 1 / 1.15;
    this.#zoom = Math.min(2.5, Math.max(0.25, this.#zoom * factor));
    this.render();
  }

  #drawTooltip(clientX = 0, clientY = 0): void {
    const tip = this.#root.querySelector<HTMLElement>(".tree-tooltip")!;
    const node = this.#hover;

    if (!node) {
      tip.hidden = true;
      return;
    }

    tip.hidden = false;
    tip.innerHTML = `
      ${node.name ? `<div class="tip-name">${esc(node.name)}</div>` : ""}
      ${node.lines.map((l) => `<div class="tip-mod">${esc(l)}</div>`).join("")}
      <div class="tip-status">${this.#status(node.id)}</div>`;

    // Offset from the cursor, and flipped near the right edge so it never
    // runs off screen.
    const rect = this.#root.getBoundingClientRect();
    const width = tip.offsetWidth;
    const x = clientX - rect.left + (clientX + width + 24 > window.innerWidth ? -width - 16 : 16);

    tip.style.left = `${x}px`;
    tip.style.top = `${clientY - rect.top + 16}px`;
  }
}

function esc(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}
