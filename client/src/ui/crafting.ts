import type { Crafting, StationMenu, RecipeOption } from "@/net/connection";

/**
 * The crafting panel: what a station can make, and what is stopping you.
 *
 * Every row says which of the two answers applies, because they are different
 * problems. A recipe you are too low for is something to work towards; one you
 * merely lack the bar for is something to go and get, and a panel that reported
 * only "cannot make" would send a player mining for a sword they could not
 * forge either way.
 *
 * The counts come from the server. A client that totalled its own bag would be
 * a client that disagreed with the database the moment anything else touched
 * the inventory, and the disagreement would surface as a recipe that looks
 * available and refuses.
 */

export interface CraftingCallbacks {
  /** Start making a recipe at the station the menu was opened on. */
  onCraft(entityId: number, recipeId: string): void;

  /** Stop whatever is running. */
  onStop(entityId: number): void;
}

export class CraftingPanel {
  #root: HTMLElement;
  #title: HTMLElement;
  #body: HTMLElement;
  #status: HTMLElement;
  #cb: CraftingCallbacks;

  /** The station this menu belongs to, and zero when closed. */
  #entityId = 0;

  /** The recipe currently running, so its row can show as active. */
  #running = "";

  /** How many of the running recipe have been made since it started. */
  #made = 0;

  constructor(root: HTMLElement, cb: CraftingCallbacks) {
    this.#root = root;
    this.#cb = cb;
    root.innerHTML =
      '<div class="craft-header">' +
      '<span class="craft-title"></span>' +
      '<span class="craft-hint">R to close</span>' +
      "</div>" +
      '<div class="craft-body"></div>' +
      '<div class="craft-status"></div>';

    this.#title = root.querySelector(".craft-title") as HTMLElement;
    this.#body = root.querySelector(".craft-body") as HTMLElement;
    this.#status = root.querySelector(".craft-status") as HTMLElement;
    root.hidden = true;
  }

  get isOpen(): boolean {
    return !this.#root.hidden;
  }

  /** The station the open menu belongs to, or zero. */
  get entityId(): number {
    return this.#entityId;
  }

  /** A station answered: show what it makes. */
  open(menu: StationMenu): void {
    this.#entityId = menu.entityId;
    this.#title.textContent = menu.name;
    this.#root.hidden = false;
    this.#render(menu.recipes);
  }

  close(): void {
    if (this.#entityId !== 0 && this.#running !== "") {
      // Closing the menu stops the run. Leaving it going would mean materials
      // being spent by a panel the player has dismissed.
      this.#cb.onStop(this.#entityId);
    }
    this.#root.hidden = true;
    this.#entityId = 0;
    this.#running = "";
    this.#made = 0;
    this.#status.textContent = "";
  }

  /** A run started, produced something, or stopped. */
  update(state: Crafting): void {
    if (state.active && state.produced) {
      this.#made++;
      this.#status.textContent = `${state.name} — made ${this.#made}`;
      this.#status.className = "craft-status making";
      return;
    }

    if (state.active) {
      this.#running = state.recipeId;
      this.#made = 0;
      this.#status.textContent = `making ${state.name}…`;
      this.#status.className = "craft-status making";
      this.#markRunning();
      return;
    }

    this.#running = "";
    this.#markRunning();

    if (state.reason) {
      // The server's words. Every reason it can refuse is a rule only it knows,
      // and running out of materials is the ordinary end of a run rather than
      // an error -- so this is information, not a failure.
      this.#status.textContent = state.reason;
      this.#status.className = "craft-status stopped";
      return;
    }

    this.#status.textContent =
      this.#made > 0 ? `made ${this.#made}` : "";
    this.#status.className = "craft-status";
  }

  #render(recipes: RecipeOption[]): void {
    this.#body.replaceChildren();

    if (recipes.length === 0) {
      const empty = document.createElement("div");
      empty.className = "craft-empty";
      empty.textContent = "nothing can be made here";
      this.#body.append(empty);
      return;
    }

    for (const rec of recipes) {
      const row = document.createElement("button");
      row.className = "craft-row";
      row.dataset.recipe = rec.recipeId;
      row.disabled = rec.blocked !== "";

      const ingredients = rec.inputs
        .map((i) => `${escapeText(i.name)} ${i.held}/${i.qty}`)
        .join(" · ");

      row.innerHTML =
        '<div class="craft-row-main">' +
        `<div class="craft-name">${escapeText(rec.name)}` +
        (rec.outputQty > 1 ? ` ×${rec.outputQty}` : "") +
        "</div>" +
        `<div class="craft-inputs">${ingredients}</div>` +
        "</div>" +
        '<div class="craft-meta">' +
        `<div class="craft-level">lv ${rec.level}</div>` +
        `<div class="craft-exp">${rec.exp} xp</div>` +
        "</div>";

      if (rec.blocked !== "") {
        const why = document.createElement("div");
        why.className = "craft-blocked";
        why.textContent = rec.blocked;
        (row.querySelector(".craft-row-main") as HTMLElement).append(why);
      }

      row.addEventListener("click", () => {
        if (this.#entityId === 0) return;
        // Clicking the running recipe stops it, which is what a player expects
        // from a button that is visibly active.
        if (this.#running === rec.recipeId) {
          this.#cb.onStop(this.#entityId);
          return;
        }
        this.#cb.onCraft(this.#entityId, rec.recipeId);
      });

      this.#body.append(row);
    }
    this.#markRunning();
  }

  #markRunning(): void {
    for (const row of this.#body.querySelectorAll<HTMLElement>(".craft-row")) {
      row.classList.toggle("running", row.dataset.recipe === this.#running);
    }
  }
}

function escapeText(s: string): string {
  const el = document.createElement("div");
  el.textContent = s;
  return el.innerHTML;
}
