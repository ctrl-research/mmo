import {
  ApiError,
  type Character,
  createCharacter,
  localLogin,
  localRegister,
  deleteCharacter,
  devLogin,
  listCharacters,
  loadProviders,
  logout,
  refreshSession,
  requestTicket,
  serverInfo,
} from "./api";

/**
 * The pre-game screens: sign in, then choose a character.
 *
 * Plain DOM rather than a framework. These are three small forms whose state
 * is entirely "which screen is showing", and the game itself renders through
 * PixiJS -- adding a UI framework here would mean shipping one to draw a login
 * box.
 */

export interface ShellCallbacks {
  /** Called with a ticket and the chosen character once the player is ready. */
  onPlay(ticket: string, character: Character, contentHash: string): void;
}

type Screen = "loading" | "login" | "characters";

export class Shell {
  #root: HTMLElement;
  #cb: ShellCallbacks;
  #contentHash = "";
  #characters: Character[] = [];
  #max = 6;
  #busy = false;

  constructor(root: HTMLElement, cb: ShellCallbacks) {
    this.#root = root;
    this.#cb = cb;
  }

  /** Shows the overlay and works out which screen the player belongs on. */
  async start(): Promise<void> {
    this.#root.hidden = false;
    this.#show("loading", "Connecting...");

    try {
      const info = await serverInfo();
      this.#contentHash = info.content;
    } catch (err) {
      this.#show("login", "", message(err));
      return;
    }

    // An expired access token is the normal state for a tab left open, so try
    // a refresh before concluding the player is signed out.
    if (!(await this.#hasSession())) {
      await this.#showLogin();
      return;
    }
    await this.#showCharacters();
  }

  /** Returns to the character list, after leaving the world. */
  async resume(reason: string): Promise<void> {
    this.#root.hidden = false;
    if (await this.#hasSession()) {
      await this.#showCharacters(reason);
    } else {
      await this.#showLogin(reason);
    }
  }

  async #hasSession(): Promise<boolean> {
    try {
      const res = await fetch("/api/me", { credentials: "same-origin" });
      if (res.ok) return true;
      if (res.status !== 401) return false;
    } catch {
      return false;
    }
    return refreshSession();
  }

  // --- login ----------------------------------------------------------------

  async #showLogin(notice = ""): Promise<void> {
    let config: { providers: { id: string; displayName: string }[]; devAuth: boolean; localAuth: boolean };
    try {
      config = await loadProviders();
    } catch (err) {
      this.#show("login", "", message(err));
      return;
    }

    const parts: string[] = [`<h1>MMO</h1><p>Sign in to play</p>`];

    for (const p of config.providers) {
      parts.push(
        `<a class="btn provider" href="/auth/login?provider=${encodeURIComponent(p.id)}">` +
          `Continue with ${escapeHTML(p.displayName)}</a>`,
      );
    }

    if (config.localAuth) {
      if (config.providers.length > 0) parts.push(`<div class="divider">or</div>`);
      parts.push(
        `<input id="username" placeholder="username" maxlength="32" autocomplete="username" />`,
        `<input id="password" type="password" placeholder="password" maxlength="256"
                autocomplete="current-password" />`,
        `<button class="btn" id="local-login">Sign in</button>`,
        `<button class="btn ghost" id="local-register">Create an account</button>`,
      );
    }

    if (config.devAuth) {
      if (config.localAuth || config.providers.length > 0) {
        parts.push(`<div class="divider">or</div>`);
      }
      parts.push(
        `<input id="dev-subject" placeholder="development sign-in name" maxlength="64" autocomplete="off" />`,
        `<button class="btn ghost" id="dev-login">Development sign-in</button>`,
        `<div class="hint">Development sign-in has no identity check. It is enabled by --dev-auth.</div>`,
      );
    }

    if (config.providers.length === 0 && !config.devAuth && !config.localAuth) {
      parts.push(
        `<div class="error">No way to sign in is configured. ` +
          `Start the server with --local-auth or --dev-auth, or configure an OIDC provider.</div>`,
      );
    }

    this.#render(parts.join(""), notice);
    this.#wireLogin(config.localAuth, config.devAuth);
  }

  #wireLogin(localAuth: boolean, devAuth: boolean): void {
    const username = this.#root.querySelector<HTMLInputElement>("#username");
    const password = this.#root.querySelector<HTMLInputElement>("#password");

    if (localAuth) {
      const submit = async (register: boolean) => {
        const u = username?.value.trim() ?? "";
        const p = password?.value ?? "";
        if (!u || !p) {
          this.#notice("Enter a username and password.", true);
          return;
        }
        await this.#guard(async () => {
          if (register) {
            await localRegister(u, p);
          } else {
            await localLogin(u, p);
          }
          localStorage.setItem("mmo.username", u);
          // Never persisted, never logged, and cleared as soon as it is used.
          if (password) password.value = "";
          await this.#showCharacters();
        }, false);
      };

      this.#root
        .querySelector("#local-login")
        ?.addEventListener("click", () => void submit(false));
      this.#root
        .querySelector("#local-register")
        ?.addEventListener("click", () => void submit(true));

      // Enter submits from either field, since a two-field form where only one
      // of them responds to Enter is quietly annoying.
      for (const el of [username, password]) {
        el?.addEventListener("keydown", (e) => {
          if (e.key === "Enter") void submit(false);
        });
      }

      if (username) {
        username.value = localStorage.getItem("mmo.username") ?? "";
        if (username.value) password?.focus();
        else username.focus();
      }
    }

    if (devAuth) {
      const subject = this.#root.querySelector<HTMLInputElement>("#dev-subject");
      const devSubmit = async () => {
        const value = subject?.value.trim() ?? "";
        if (!value) {
          this.#notice("Enter a name to sign in with.", true);
          return;
        }
        await this.#guard(async () => {
          await devLogin(value);
          localStorage.setItem("mmo.subject", value);
          await this.#showCharacters();
        }, false);
      };

      this.#root.querySelector("#dev-login")?.addEventListener("click", () => void devSubmit());
      subject?.addEventListener("keydown", (e) => {
        if (e.key === "Enter") void devSubmit();
      });
      if (subject) subject.value = localStorage.getItem("mmo.subject") ?? "";

      if (!localAuth) subject?.focus();
    }
  }

  // --- characters -----------------------------------------------------------

  async #showCharacters(notice = ""): Promise<void> {
    try {
      const res = await listCharacters();
      this.#characters = res.characters;
      this.#max = res.max;
    } catch (err) {
      await this.#showLogin(message(err));
      return;
    }

    const rows = this.#characters
      .map(
        (c) => `
        <li class="char" data-id="${c.id}">
          <div class="char-main">
            <div class="char-name">${escapeHTML(c.name)}</div>
            <div class="char-sub">level ${c.level} &middot; ${escapeHTML(c.class)} &middot; ${escapeHTML(c.mapId)}</div>
          </div>
          <button class="btn small play" data-id="${c.id}">Play</button>
          <button class="btn small ghost delete" data-id="${c.id}" title="Delete">&times;</button>
        </li>`,
      )
      .join("");

    const canCreate = this.#characters.length < this.#max;

    this.#render(
      `<h1>Characters</h1>
       <p>${this.#characters.length} of ${this.#max}</p>
       ${rows ? `<ul class="chars">${rows}</ul>` : `<p class="hint">No characters yet.</p>`}
       ${
         canCreate
           ? `<input id="new-name" placeholder="new character name" maxlength="16" autocomplete="off" />
              <button class="btn" id="create">Create</button>`
           : `<div class="hint">Delete one to make room for another.</div>`
       }
       <button class="btn ghost" id="sign-out">Sign out</button>`,
      notice,
    );

    this.#root.querySelectorAll<HTMLButtonElement>(".play").forEach((b) =>
      b.addEventListener("click", () => void this.#play(b.dataset.id!)),
    );
    this.#root.querySelectorAll<HTMLButtonElement>(".delete").forEach((b) =>
      b.addEventListener("click", () => void this.#delete(b.dataset.id!)),
    );

    const name = this.#root.querySelector<HTMLInputElement>("#new-name");
    const create = async () => {
      const value = name?.value.trim() ?? "";
      if (!value) {
        this.#notice("Enter a name.", true);
        return;
      }
      await this.#guard(async () => {
        await createCharacter(value);
        await this.#showCharacters();
      });
    };
    this.#root.querySelector("#create")?.addEventListener("click", () => void create());
    name?.addEventListener("keydown", (e) => {
      if (e.key === "Enter") void create();
    });

    this.#root.querySelector("#sign-out")?.addEventListener("click", () =>
      void this.#guard(async () => {
        await logout();
        await this.#showLogin("Signed out.");
      }),
    );

    name?.focus();
  }

  async #play(characterId: string): Promise<void> {
    const character = this.#characters.find((c) => c.id === characterId);
    if (!character) return;

    await this.#guard(async () => {
      const ticket = await requestTicket(characterId);
      this.#root.hidden = true;
      this.#cb.onPlay(ticket, character, this.#contentHash);
    });
  }

  async #delete(characterId: string): Promise<void> {
    const character = this.#characters.find((c) => c.id === characterId);
    if (!character) return;

    // Deletion frees the name for anyone to take, so it is worth one
    // confirmation rather than a single mis-click.
    if (!confirm(`Delete ${character.name}? This cannot be undone.`)) return;

    await this.#guard(async () => {
      await deleteCharacter(characterId);
      await this.#showCharacters(`${character.name} deleted.`);
    });
  }

  // --- plumbing -------------------------------------------------------------

  /**
   * Runs an action with the buttons disabled, surfacing any error.
   *
   * Without this a double-click creates two characters, and a failure leaves
   * the panel looking like nothing happened.
   *
   * `signedIn` says whether a 401 means the session lapsed. It does for calls
   * that require a session -- but a sign-in attempt returns 401 for a wrong
   * password, and treating that as an expiry replaces the real message with
   * "your session expired" and re-renders the form, wiping what was typed.
   */
  async #guard(fn: () => Promise<void>, signedIn = true): Promise<void> {
    if (this.#busy) return;
    this.#busy = true;
    this.#setDisabled(true);

    try {
      await fn();
    } catch (err) {
      if (signedIn && err instanceof ApiError && err.status === 401) {
        await this.#showLogin("Your session expired. Sign in again.");
      } else {
        this.#notice(message(err), true);
      }
    } finally {
      this.#busy = false;
      this.#setDisabled(false);
    }
  }

  #setDisabled(disabled: boolean): void {
    this.#root.querySelectorAll<HTMLButtonElement>("button").forEach((b) => {
      b.disabled = disabled;
    });
  }

  #show(_screen: Screen, text: string, error = ""): void {
    this.#render(`<h1>MMO</h1><p>${escapeHTML(text)}</p>`, error, true);
  }

  #render(inner: string, notice = "", isError = false): void {
    this.#root.innerHTML = `<div class="panel">${inner}<div class="notice${
      isError || notice ? "" : " empty"
    }" id="notice"></div></div>`;
    if (notice) this.#notice(notice, isError);
  }

  #notice(text: string, isError = false): void {
    const el = this.#root.querySelector<HTMLDivElement>("#notice");
    if (!el) return;
    el.textContent = text;
    el.className = isError ? "notice error" : "notice";
  }
}

/** Escapes text for interpolation into markup. */
function escapeHTML(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
