import type { ItemStack } from "@/gen/mmo/v1/game_pb";
import {
  EQUIP_SLOTS,
  formatDelta,
  formatMod,
  formatStat,
  InventoryState,
  predictChange,
  SLOT_LABELS,
  statLabel,
  STAT_SCALE,
} from "@/game/inventory";

/**
 * The inventory panel: carried items, equipment, and the character's stats.
 *
 * The tooltip is the point of this screen. The milestone's exit criterion is
 * that equipping an item changes damage by exactly the amount the tooltip
 * predicted, so the prediction has to be shown, and it has to be right --
 * which is why it is computed from the item's own rolled modifiers, the same
 * values the server used, rather than from a client-side reimplementation of
 * the stat pipeline.
 */

export type ItemActionKind = "equip" | "unequip" | "destroy" | "move";

export interface InventoryCallbacks {
  onAction(kind: ItemActionKind, item: ItemStack | null, slot?: number, equipSlot?: string): void;
}

/** Stats shown on the character panel, in reading order. */
const SHOWN_STATS = [
  "attack",
  "armour",
  "max_life",
  "max_mana",
  "crit_chance",
  "crit_multiplier",
  "attack_speed",
  "movement_speed",
  "strength",
  "dexterity",
  "intelligence",
  "fire_resistance",
];

export class InventoryPanel {
  #root: HTMLElement;
  #tooltip: HTMLElement;
  #state: InventoryState;
  #cb: InventoryCallbacks;
  #open = false;

  constructor(root: HTMLElement, tooltip: HTMLElement, state: InventoryState, cb: InventoryCallbacks) {
    this.#root = root;
    this.#tooltip = tooltip;
    this.#state = state;
    this.#cb = cb;

    // A tooltip that outlives the thing it describes is worse than none: it
    // sits over the game claiming something that is no longer true.
    root.addEventListener("mouseleave", () => this.#hideTooltip());
  }

  get isOpen(): boolean {
    return this.#open;
  }

  toggle(): void {
    this.#open = !this.#open;
    this.#root.hidden = !this.#open;
    this.#hideTooltip();
    if (this.#open) this.render();
  }

  close(): void {
    this.#open = false;
    this.#root.hidden = true;
    this.#hideTooltip();
  }

  /** Redraws from the current state. Called whenever the server sends one. */
  render(): void {
    if (!this.#open) return;

    const stats = this.#state.stats();

    this.#root.innerHTML = `
      <div class="inv-header">
        <span>Inventory</span>
        <span class="inv-hint">click to equip &middot; shift-click to destroy &middot; [i] to close</span>
      </div>
      <div class="inv-body">
        <div class="inv-equipment">
          ${EQUIP_SLOTS.map((slot) => this.#equipSlotHTML(slot)).join("")}
        </div>
        <div class="inv-grid">
          ${this.#gridHTML()}
        </div>
        <div class="inv-stats">
          ${SHOWN_STATS.map(
            (s) => `<div class="stat-row"><span>${statLabel(s)}</span><b>${formatStat(
              s,
              stats[s] ?? 0,
            )}</b></div>`,
          ).join("")}
        </div>
      </div>`;

    this.#wire();
  }

  #equipSlotHTML(slot: string): string {
    const item = this.#state.equippedIn(slot);
    const label = SLOT_LABELS[slot] ?? slot;

    if (!item) {
      return `<div class="equip-slot empty" data-equip-slot="${slot}">
                <span class="slot-label">${label}</span>
              </div>`;
    }
    return `<div class="equip-slot rarity-${item.rarity}"
                 data-equip-slot="${slot}" data-item-id="${item.itemId}">
              <span class="slot-label">${label}</span>
              <span class="item-name">${escapeHTML(item.name)}</span>
            </div>`;
  }

  #gridHTML(): string {
    const cells: string[] = [];
    for (let slot = 0; slot < this.#state.capacity; slot++) {
      const item = this.#state.itemAt(slot);
      if (!item) {
        cells.push(`<div class="inv-cell empty" data-slot="${slot}"></div>`);
        continue;
      }
      cells.push(
        `<div class="inv-cell rarity-${item.rarity}" data-slot="${slot}" data-item-id="${item.itemId}">
           <span class="cell-name">${escapeHTML(shortName(item.name))}</span>
           ${item.stack > 1 ? `<span class="cell-stack">${item.stack}</span>` : ""}
         </div>`,
      );
    }
    return cells.join("");
  }

  #wire(): void {
    this.#root.querySelectorAll<HTMLElement>(".inv-cell[data-item-id]").forEach((el) => {
      const item = this.#state.carried.find((i) => i.itemId === el.dataset.itemId);
      if (!item) return;

      el.addEventListener("mouseenter", (e) => this.#showTooltip(item, e));
      el.addEventListener("mousemove", (e) => this.#positionTooltip(e));
      el.addEventListener("mouseleave", () => this.#hideTooltip());
      el.addEventListener("click", (e) => {
        this.#hideTooltip();
        if (e.shiftKey) {
          if (confirm(`Destroy ${item.name}? This cannot be undone.`)) {
            this.#cb.onAction("destroy", item);
          }
          return;
        }
        if (item.equipSlot) this.#cb.onAction("equip", item);
      });
    });

    this.#root.querySelectorAll<HTMLElement>(".equip-slot[data-item-id]").forEach((el) => {
      const slot = el.dataset.equipSlot!;
      const item = this.#state.equippedIn(slot);
      if (!item) return;

      el.addEventListener("mouseenter", (e) => this.#showTooltip(item, e, true));
      el.addEventListener("mousemove", (e) => this.#positionTooltip(e));
      el.addEventListener("mouseleave", () => this.#hideTooltip());
      el.addEventListener("click", () => {
        this.#hideTooltip();
        this.#cb.onAction("unequip", null, undefined, slot);
      });
    });
  }

  #showTooltip(item: ItemStack, e: MouseEvent, equipped = false): void {
    const lines: string[] = [
      `<div class="tt-name rarity-${item.rarity}">${escapeHTML(item.name)}</div>`,
      `<div class="tt-sub">${item.rarity}${
        item.equipSlot ? ` &middot; ${SLOT_LABELS[item.equipSlot] ?? item.equipSlot}` : ""
      } &middot; item level ${item.itemLevel}</div>`,
    ];

    if (item.requiredLevel > 1) {
      lines.push(`<div class="tt-req">Requires level ${item.requiredLevel}</div>`);
    }

    const implicits = item.mods.filter((m) => m.implicit);
    const affixes = item.mods.filter((m) => !m.implicit);

    if (implicits.length) {
      lines.push(
        `<div class="tt-mods implicit">${implicits
          .map((m) => `<div>${escapeHTML(formatMod(m))}</div>`)
          .join("")}</div>`,
      );
    }
    if (affixes.length) {
      lines.push(
        `<div class="tt-mods">${affixes
          .map(
            (m) =>
              `<div>${escapeHTML(formatMod(m))}${
                // The tier says how good a roll is, not merely what it does --
                // the difference between "this is fine" and "this is worth
                // keeping".
                m.tier ? `<span class="tt-tier">T${m.tier}</span>` : ""
              }</div>`,
          )
          .join("")}</div>`,
      );
    }

    // The prediction. Only shown for something not already worn, since
    // predicting the effect of equipping what is equipped is noise.
    if (!equipped && item.equipSlot) {
      const replacing = this.#state.equippedIn(item.equipSlot);
      const { flat, percent } = predictChange(item, replacing);

      const rows: string[] = [];
      for (const [stat, delta] of Object.entries(flat)) {
        rows.push(
          `<div class="${delta >= 0 ? "up" : "down"}">${formatDelta(stat, delta)} ${statLabel(
            stat,
          )}</div>`,
        );
      }
      for (const [stat, delta] of Object.entries(percent)) {
        rows.push(
          `<div class="${delta >= 0 ? "up" : "down"}">${
            delta >= 0 ? "+" : ""
          }${((delta / STAT_SCALE) * 100).toFixed(0)}% ${statLabel(stat)}</div>`,
        );
      }

      lines.push(
        rows.length
          ? `<div class="tt-change"><div class="tt-change-label">${
              replacing ? "Replacing what is worn" : "If equipped"
            }</div>${rows.join("")}</div>`
          : `<div class="tt-change"><div class="tt-change-label">No change</div></div>`,
      );
    }

    this.#tooltip.innerHTML = lines.join("");
    this.#tooltip.hidden = false;
    this.#positionTooltip(e);
  }

  #positionTooltip(e: MouseEvent): void {
    if (this.#tooltip.hidden) return;

    // Kept inside the viewport, so an item near the right edge does not push
    // its own tooltip off screen.
    const pad = 14;
    const rect = this.#tooltip.getBoundingClientRect();
    let x = e.clientX + pad;
    let y = e.clientY + pad;

    if (x + rect.width > window.innerWidth) x = e.clientX - rect.width - pad;
    if (y + rect.height > window.innerHeight) y = window.innerHeight - rect.height - pad;

    this.#tooltip.style.left = `${Math.max(pad, x)}px`;
    this.#tooltip.style.top = `${Math.max(pad, y)}px`;
  }

  #hideTooltip(): void {
    this.#tooltip.hidden = true;
  }
}

/** Trims a name to fit an inventory cell. */
function shortName(name: string): string {
  return name.length > 22 ? name.slice(0, 21) + "…" : name;
}

function escapeHTML(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}
