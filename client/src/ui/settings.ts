import { DEFAULT_ZOOM, MAX_ZOOM, MIN_ZOOM } from "@/render/scene";

/**
 * Client settings.
 *
 * Everything here is per-browser and per-person: it describes how one player
 * wants to look at the game, not anything the server has an opinion about. So
 * it lives in localStorage and is never sent anywhere.
 *
 * Reads are wrapped because storage can throw outright -- a private window, a
 * browser set to block site data -- and a settings panel that took the game
 * down with it would be a poor trade for remembering a zoom level.
 */

const ZOOM_KEY = "mmo.zoom";
const AUTOLOOT_KEY = "mmo.autoloot";

/** Returns the saved zoom, or the default when there is none or it is junk. */
export function loadZoom(): number {
  try {
    const raw = localStorage.getItem(ZOOM_KEY);
    if (raw === null) return DEFAULT_ZOOM;
    const value = Number(raw);
    if (!Number.isFinite(value)) return DEFAULT_ZOOM;
    return Math.min(Math.max(value, MIN_ZOOM), MAX_ZOOM);
  } catch {
    return DEFAULT_ZOOM;
  }
}

/** Returns whether automatic looting is on. Off unless it was turned on. */
export function loadAutoLoot(): boolean {
  try {
    return localStorage.getItem(AUTOLOOT_KEY) === "1";
  } catch {
    return false;
  }
}

function saveAutoLoot(on: boolean): void {
  try {
    localStorage.setItem(AUTOLOOT_KEY, on ? "1" : "0");
  } catch {
    // As above: not remembering it is no reason not to apply it.
  }
}

function saveZoom(zoom: number): void {
  try {
    localStorage.setItem(ZOOM_KEY, String(zoom));
  } catch {
    // Not being able to remember it is not a reason to refuse to apply it.
  }
}

export interface SettingsCallbacks {
  /** Applies a zoom and returns what it settled on after clamping. */
  onZoom(zoom: number): number;

  /** Turns automatic looting on or off. */
  onAutoLoot(on: boolean): void;
}

export class SettingsPanel {
  #root: HTMLElement;
  #cb: SettingsCallbacks;
  #slider: HTMLInputElement;
  #value: HTMLElement;
  #autoLoot: HTMLInputElement;

  constructor(root: HTMLElement, cb: SettingsCallbacks) {
    this.#root = root;
    this.#cb = cb;

    root.innerHTML =
      '<div class="settings-header">Settings<span class="settings-hint">[k] to close</span></div>' +
      '<label class="settings-row">' +
      '<span class="settings-label">Camera zoom</span>' +
      `<input class="settings-slider" type="range" min="${MIN_ZOOM}" max="${MAX_ZOOM}" step="0.25">` +
      '<span class="settings-value"></span>' +
      "</label>" +
      '<p class="settings-note">How close the camera sits.</p>' +
      '<label class="settings-row">' +
      '<span class="settings-label">Auto-loot</span>' +
      '<input class="settings-autoloot" type="checkbox">' +
      '<span class="settings-value"></span>' +
      "</label>" +
      '<p class="settings-note">Picks up drops you walk over. Hold Z to loot by hand either way. Saved in this browser.</p>';

    this.#slider = root.querySelector(".settings-slider") as HTMLInputElement;
    this.#value = root.querySelector(".settings-value") as HTMLElement;
    this.#autoLoot = root.querySelector(".settings-autoloot") as HTMLInputElement;

    this.#slider.addEventListener("input", () => {
      this.#apply(Number(this.#slider.value));
    });
    this.#autoLoot.addEventListener("change", () => {
      this.#applyAutoLoot(this.#autoLoot.checked);
    });

    root.hidden = true;
  }

  get isOpen(): boolean {
    return !this.#root.hidden;
  }

  /** Applies the saved settings. Called once, as the world is entered. */
  restore(): void {
    this.#apply(loadZoom());
    this.#applyAutoLoot(loadAutoLoot());
  }

  toggle(): void {
    this.#root.hidden = !this.#root.hidden;
    if (!this.isOpen) return;
    this.#slider.value = String(this.#cb.onZoom(loadZoom()));
    this.#autoLoot.checked = loadAutoLoot();
  }

  close(): void {
    this.#root.hidden = true;
  }

  #applyAutoLoot(on: boolean): void {
    this.#autoLoot.checked = on;
    this.#cb.onAutoLoot(on);
    saveAutoLoot(on);
  }

  #apply(zoom: number): void {
    // The scene is the authority on what it settled on, so the slider and the
    // saved value both follow it rather than the other way round -- otherwise
    // a value the camera clamped would be remembered as though it had worked.
    const applied = this.#cb.onZoom(zoom);
    this.#slider.value = String(applied);
    this.#value.textContent = `${applied.toFixed(2)}x`;
    saveZoom(applied);
  }
}
