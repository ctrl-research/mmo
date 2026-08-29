import type { Inventory, ItemStack, ItemMod } from "@/gen/mmo/v1/game_pb";

/**
 * The client's view of the inventory.
 *
 * Purely a cache of what the server sent. Nothing here decides anything: the
 * server owns where items are and what they do, and this exists so the UI can
 * be drawn without a round trip.
 */

/** Stat values arrive in millionths, matching the server's representation. */
export const STAT_SCALE = 1_000_000;

export interface StatMap {
  [stat: string]: number;
}

export class InventoryState {
  #inventory: Inventory | null = null;

  /** Replaces the whole inventory. The server never sends a partial one. */
  apply(inv: Inventory): void {
    this.#inventory = inv;
  }

  get raw(): Inventory | null {
    return this.#inventory;
  }

  get carried(): ItemStack[] {
    return this.#inventory?.carried ?? [];
  }

  get equipped(): ItemStack[] {
    return this.#inventory?.equipped ?? [];
  }

  get capacity(): number {
    return this.#inventory?.capacity ?? 0;
  }

  /** Current derived stats, keyed by name. */
  stats(): StatMap {
    const out: StatMap = {};
    for (const s of this.#inventory?.stats ?? []) {
      out[s.stat] = Number(s.value);
    }
    return out;
  }

  itemAt(slot: number): ItemStack | undefined {
    return this.carried.find((i) => i.slot === slot);
  }

  equippedIn(slot: string): ItemStack | undefined {
    return this.equipped.find((i) => i.equipSlot === slot);
  }
}

/**
 * Predicts how equipping an item would change the character's stats.
 *
 * This is the number the exit criterion turns on: the tooltip says what will
 * happen, and equipping must then do exactly that. It is computed from the
 * item's own rolled modifiers -- the same values the server used -- rather
 * than from a client-side reimplementation of the stat pipeline, so the two
 * cannot drift.
 *
 * Only flat modifiers are predicted exactly. An "increased" modifier scales
 * whatever the total happens to be, so its effect depends on everything else
 * equipped; showing a precise number for it would be a number that is
 * sometimes wrong, which is worse than showing the percentage itself.
 */
export function predictChange(
  item: ItemStack,
  replacing: ItemStack | undefined,
): { flat: StatMap; percent: StatMap } {
  const flat: StatMap = {};
  const percent: StatMap = {};

  const apply = (mods: ItemMod[], sign: number) => {
    for (const m of mods) {
      const value = Number(m.value) * sign;
      if (m.kind === "flat") {
        flat[m.stat] = (flat[m.stat] ?? 0) + value;
      } else {
        percent[m.stat] = (percent[m.stat] ?? 0) + value;
      }
    }
  };

  apply(item.mods, 1);
  // Equipping replaces whatever is worn, so its modifiers come back off.
  if (replacing) apply(replacing.mods, -1);

  for (const key of Object.keys(flat)) {
    if (flat[key] === 0) delete flat[key];
  }
  for (const key of Object.keys(percent)) {
    if (percent[key] === 0) delete percent[key];
  }
  return { flat, percent };
}

/** Formats a stat value for display. */
export function formatStat(stat: string, value: number): string {
  if (isPercentStat(stat)) {
    return `${(value / STAT_SCALE * 100).toFixed(0)}%`;
  }
  return (value / STAT_SCALE).toFixed(0);
}

/** Formats a modifier line as it appears on a tooltip. */
export function formatMod(m: ItemMod): string {
  const name = statLabel(m.stat);
  const value = Number(m.value);

  switch (m.kind) {
    case "flat":
      return `${formatDelta(m.stat, value)} ${name}`;
    case "increased":
      return `${signed((value / STAT_SCALE) * 100, true)} increased ${name}`;
    case "more":
      return `${signed((value / STAT_SCALE) * 100, true)} more ${name}`;
    default:
      return `${name} ${value}`;
  }
}

/**
 * Formats a flat change to a stat.
 *
 * A flat modifier to a percentage stat is stored as a fraction: +18% critical
 * multiplier is 0.18. Rendering that without scaling shows "+0%", which reads
 * as an item with a worthless modifier rather than a formatting mistake --
 * exactly the kind of wrong number a player would plan around.
 */
export function formatDelta(stat: string, value: number): string {
  const v = value / STAT_SCALE;
  return isPercentStat(stat) ? signed(v * 100, true) : signed(v, false);
}

function signed(v: number, percent: boolean): string {
  // One decimal where the whole number would hide the value entirely, so a
  // small but real modifier does not read as zero.
  const abs = Math.abs(v);
  const text = abs > 0 && abs < 1 ? v.toFixed(1) : v.toFixed(0);
  return (v >= 0 ? "+" : "") + text + (percent ? "%" : "");
}

/** Stats whose values read as percentages rather than points. */
function isPercentStat(stat: string): boolean {
  return (
    stat.endsWith("_resistance") ||
    stat === "crit_chance" ||
    stat === "crit_multiplier" ||
    stat === "life_leech"
  );
}

const STAT_LABELS: Record<string, string> = {
  attack: "Attack",
  armour: "Armour",
  max_life: "Life",
  max_mana: "Mana",
  attack_speed: "Attack Speed",
  crit_chance: "Critical Chance",
  crit_multiplier: "Critical Multiplier",
  movement_speed: "Movement Speed",
  strength: "Strength",
  dexterity: "Dexterity",
  intelligence: "Intelligence",
  fire_resistance: "Fire Resistance",
  cold_resistance: "Cold Resistance",
  lightning_resistance: "Lightning Resistance",
  life_leech: "Life Leech",
};

export function statLabel(stat: string): string {
  return STAT_LABELS[stat] ?? stat;
}

/** Equipment slots, in the order the paperdoll shows them. */
export const EQUIP_SLOTS = ["weapon", "helmet", "chest", "gloves", "boots", "ring"] as const;

export const SLOT_LABELS: Record<string, string> = {
  weapon: "Weapon",
  helmet: "Helmet",
  chest: "Chest",
  gloves: "Gloves",
  boots: "Boots",
  ring: "Ring",
};
