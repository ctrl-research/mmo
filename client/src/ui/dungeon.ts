import type { DungeonState } from "@/net/connection";

/**
 * The dungeon frame: where the run stands.
 *
 * A party needs to know two things a health bar cannot tell them — which stage
 * they are on, and whether the run is still winnable. Without it, a stage that
 * has opened and a stage that has not are the same empty room, and the
 * difference is whether they should be pushing forward or holding.
 */

/** How long a stage change stays lit. */
const ANNOUNCE_MS = 5_000;

export class DungeonFrame {
  #root: HTMLElement;
  #name: HTMLElement;
  #stage: HTMLElement;
  #note: HTMLElement;

  #announcedAt = 0;

  /** When the party is sent home, in page time; 0 while the run is active. */
  #endsAt = 0;

  constructor(root: HTMLElement) {
    this.#root = root;
    root.innerHTML =
      '<div class="dungeon-name"></div>' +
      '<div class="dungeon-stage"></div>' +
      '<div class="dungeon-note"></div>';

    this.#name = root.querySelector(".dungeon-name") as HTMLElement;
    this.#stage = root.querySelector(".dungeon-stage") as HTMLElement;
    this.#note = root.querySelector(".dungeon-note") as HTMLElement;
    root.hidden = true;
  }

  update(state: DungeonState, now: number): void {
    this.#announcedAt = now;
    this.#root.hidden = false;

    this.#name.textContent = state.name;
    this.#stage.textContent = `${state.stageName} — ${state.stage}/${state.stages}`;

    const over = state.state !== "active";
    this.#root.classList.toggle("cleared", state.state === "cleared");
    this.#root.classList.toggle("wiped", state.state === "wiped");

    this.#endsAt = over ? now + state.endsInMs : 0;
    this.#note.hidden = !over;
  }

  /** Called every frame: fades the stage line and counts down the exit. */
  render(now: number): void {
    if (this.#root.hidden) return;

    this.#stage.classList.toggle("fresh", now - this.#announcedAt < ANNOUNCE_MS);

    if (this.#endsAt === 0) return;
    const left = Math.max(0, this.#endsAt - now);
    this.#note.textContent =
      left > 0 ? `leaving in ${Math.ceil(left / 1000)}s` : "leaving...";
  }

  /** The character left the instance, one way or another. */
  hide(): void {
    this.#root.hidden = true;
    this.#root.classList.remove("cleared", "wiped");
    this.#endsAt = 0;
  }
}
