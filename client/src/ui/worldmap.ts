import type { WorldMap } from "@/net/connection";

/**
 * The world map: where the character can go, and where they are.
 *
 * Everything on it is a request, never a fact the client asserts. The server
 * decides whether a waypoint is unlocked and whether a channel has room, so a
 * button here is a question, and a refusal comes back as a message rather than
 * as the screen quietly disagreeing with the world.
 */

export interface WorldMapCallbacks {
  /** Asks the server for fresh contents. */
  onRefresh(): void;

  onTravel(waypointId: string): void;
  onSwitchChannel(instanceId: bigint): void;
  onNewChannel(): void;
}

export class WorldMapPanel {
  #root: HTMLElement;
  #cb: WorldMapCallbacks;
  #open = false;
  #map: WorldMap | null = null;

  constructor(root: HTMLElement, cb: WorldMapCallbacks) {
    this.#root = root;
    this.#cb = cb;
  }

  get isOpen(): boolean {
    return this.#open;
  }

  toggle(): void {
    this.#open = !this.#open;
    this.#root.hidden = !this.#open;
    if (this.#open) {
      // Always refetched rather than shown from cache: a channel that was
      // half empty a minute ago is the one piece of this screen that goes
      // stale on its own.
      this.#cb.onRefresh();
      this.render();
    }
  }

  close(): void {
    this.#open = false;
    this.#root.hidden = true;
  }

  /** Replaces the contents with what the server sent. */
  update(map: WorldMap): void {
    this.#map = map;
    if (this.#open) this.render();
  }

  render(): void {
    if (!this.#open) return;

    const m = this.#map;
    if (!m) {
      this.#root.innerHTML = `<div class="map-header">World map</div>
        <div class="map-body"><p class="map-empty">loading...</p></div>`;
      return;
    }

    this.#root.innerHTML = `
      <div class="map-header">
        <span>World map</span>
        <span class="map-hint">click a waypoint to travel &middot; M to close</span>
      </div>
      <div class="map-body">
        <section>
          <h2>Zones</h2>
          ${m.maps.map((z) => this.#zone(z, m.currentMapId)).join("")}
        </section>
        <section>
          <h2>Waypoints</h2>
          ${
            m.waypoints.length === 0
              ? `<p class="map-empty">None found yet. Walk over one to unlock it.</p>`
              : m.waypoints
                  .map(
                    (w) => `<button class="map-row action" data-waypoint="${esc(w.waypointId)}">
                        <span class="map-name">${esc(w.name)}</span>
                        <span class="map-sub">${esc(w.mapId)}</span>
                      </button>`,
                  )
                  .join("")
          }
        </section>
        <section>
          <h2>Channels</h2>
          ${
            m.channels.length === 0
              ? `<p class="map-empty">This area is not channelled.</p>`
              : m.channels
                  .map(
                    (c) => `<button class="map-row action${c.current ? " current" : ""}"
                        data-channel="${c.instanceId}" ${c.current ? "disabled" : ""}>
                        <span class="map-name">Channel ${c.channel}</span>
                        <span class="map-sub">${c.players}/${c.capacity}${
                          c.current ? " &middot; you are here" : ""
                        }</span>
                      </button>`,
                  )
                  .join("")
          }
          ${
            m.channels.length === 0
              ? ""
              : `<button class="map-row action" data-new-channel="1">
                   <span class="map-name">New channel</span>
                   <span class="map-sub">a fresh instance of this area</span>
                 </button>`
          }
        </section>
      </div>`;

    for (const el of this.#root.querySelectorAll<HTMLButtonElement>("[data-waypoint]")) {
      el.addEventListener("click", () => this.#cb.onTravel(el.dataset.waypoint!));
    }
    for (const el of this.#root.querySelectorAll<HTMLButtonElement>("[data-channel]")) {
      el.addEventListener("click", () => this.#cb.onSwitchChannel(BigInt(el.dataset.channel!)));
    }
    for (const el of this.#root.querySelectorAll<HTMLButtonElement>("[data-new-channel]")) {
      el.addEventListener("click", () => this.#cb.onNewChannel());
    }
  }

  /**
   * One zone.
   *
   * Level ranges are advisory -- the portal's own requirement is what actually
   * gates entry -- but they are the whole reason to look at this screen: they
   * say where to go next.
   */
  #zone(z: WorldMap["maps"][number], currentMapId: string): string {
    const here = z.mapId === currentMapId;
    const range = z.maxLevel > 0 ? `lv ${z.minLevel}&ndash;${z.maxLevel}` : `lv ${z.minLevel}+`;
    const kind = z.private ? "dungeon" : "";

    return `<div class="map-row${here ? " current" : ""}">
      <span class="map-name">${esc(z.name || z.mapId)}</span>
      <span class="map-sub">${range}${kind ? ` &middot; ${kind}` : ""}${
        here ? " &middot; you are here" : ""
      }</span>
    </div>`;
  }
}

/** Escapes text for interpolation into markup. */
function esc(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}
