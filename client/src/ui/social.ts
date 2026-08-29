import { PartyAction_Kind, GuildAction_Kind, SocialAction_Kind } from "@/net/connection";
import type { PartyState, GuildState, FriendList } from "@/net/connection";

/**
 * Party frames, the guild roster, and the friends list.
 *
 * The party frames sit on screen permanently and everything else lives behind
 * a key, because they answer different questions. A member frame answers "is
 * my healer about to die", which is only useful if it is already visible; a
 * roster answers "who is around", which is worth opening a panel for.
 */

export interface SocialCallbacks {
  onParty(kind: PartyAction_Kind, target?: string): void;
  onGuild(kind: GuildAction_Kind, target?: string): void;
  onFriends(kind: SocialAction_Kind, target?: string): void;

  /** Asks for a name, for the actions that need one. */
  prompt(question: string): Promise<string | null>;
}

export class PartyFrames {
  #root: HTMLElement;
  #cb: SocialCallbacks;
  #state: PartyState | null = null;

  constructor(root: HTMLElement, cb: SocialCallbacks) {
    this.#root = root;
    this.#cb = cb;
  }

  update(state: PartyState): void {
    // An empty roster means "not in a party".
    this.#state = state.members.length > 0 ? state : null;
    this.render();
  }

  render(): void {
    this.#root.hidden = false;

    const s = this.#state;
    if (!s) {
      // Shown even with no party, because the only way to start one is to
      // invite somebody, and hiding the panel until you are in a party leaves
      // nowhere to do that from.
      this.#root.innerHTML = `
        <div class="party-header"><span>Party</span></div>
        <div class="party-actions">
          <button data-party="invite">Invite someone</button>
        </div>`;
      this.#bindInvite();
      return;
    }

    this.#root.innerHTML = `
      <div class="party-header">
        <span>Party</span>
        <span class="party-loot">${esc(s.loot)}</span>
      </div>
      ${s.members.map((m) => frame(m, s)).join("")}
      <div class="party-actions">
        <button data-party="invite">Invite</button>
        <button data-party="leave">Leave</button>
        ${
          s.leaderCharacterId === s.selfCharacterId
            ? `<button data-party="loot">Loot rule</button>`
            : ""
        }
      </div>`;

    const act: Record<string, () => void> = {
      invite: async () => {
        const name = await this.#cb.prompt("Invite who?");
        if (name) this.#cb.onParty(PartyAction_Kind.INVITE, name);
      },
      leave: () => this.#cb.onParty(PartyAction_Kind.LEAVE),
      loot: () => {
        const next = s.loot === "round-robin" ? "free-for-all" : "round-robin";
        this.#cb.onParty(PartyAction_Kind.SET_LOOT, next);
      },
    };
    for (const el of this.#root.querySelectorAll<HTMLButtonElement>("[data-party]")) {
      el.addEventListener("click", () => act[el.dataset.party!]?.());
    }

    for (const el of this.#root.querySelectorAll<HTMLElement>("[data-kick]")) {
      el.addEventListener("click", () =>
        this.#cb.onParty(PartyAction_Kind.KICK, el.dataset.kick!),
      );
    }
  }

  #bindInvite(): void {
    this.#root.querySelector("[data-party=invite]")!.addEventListener("click", async () => {
      const name = await this.#cb.prompt("Invite who?");
      if (name) this.#cb.onParty(PartyAction_Kind.INVITE, name);
    });
  }
}

/**
 * One member frame.
 *
 * Health as a bar rather than a number: the question it answers is "how close
 * to dead", and a bar answers it without being read.
 */
function frame(m: PartyState["members"][number], s: PartyState): string {
  const pct = m.hpMax > 0 ? Math.round((m.hp / m.hpMax) * 100) : 0;
  const self = m.characterId === s.selfCharacterId;
  const leader = m.characterId === s.leaderCharacterId;
  const canKick = s.leaderCharacterId === s.selfCharacterId && !self;

  return `<div class="party-member${m.online ? "" : " offline"}">
    <div class="party-name">
      ${leader ? "&#9733; " : ""}${esc(m.name)}
      <span class="party-where">${esc(m.mapId)}</span>
      ${canKick ? `<button class="party-kick" data-kick="${esc(m.name)}">&times;</button>` : ""}
    </div>
    <div class="party-bar"><div class="party-bar-fill" style="width:${pct}%"></div></div>
  </div>`;
}

export class SocialPanel {
  #root: HTMLElement;
  #cb: SocialCallbacks;
  #open = false;

  #guild: GuildState | null = null;
  #friends: FriendList | null = null;

  constructor(root: HTMLElement, cb: SocialCallbacks) {
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
      // The friends list is the half that goes stale on its own: somebody logs
      // in and the panel would keep saying they are not.
      this.#cb.onFriends(SocialAction_Kind.LIST_FRIENDS);
      this.render();
    }
  }

  close(): void {
    this.#open = false;
    this.#root.hidden = true;
  }

  updateGuild(state: GuildState): void {
    this.#guild = state.guildId === "" ? null : state;
    if (this.#open) this.render();
  }

  updateFriends(list: FriendList): void {
    this.#friends = list;
    if (this.#open) this.render();
  }

  render(): void {
    if (!this.#open) return;

    this.#root.innerHTML = `
      <div class="social-header">
        <span>Social</span>
        <span class="social-hint">O to close</span>
      </div>
      <div class="social-body">
        <section>${this.#guildSection()}</section>
        <section>${this.#friendsSection()}</section>
      </div>`;

    this.#bind();
  }

  #guildSection(): string {
    const g = this.#guild;
    if (!g) {
      return `<h2>Guild</h2>
        <p class="social-empty">You are not in a guild.</p>
        <button class="social-action" data-guild="create">Found a guild</button>`;
    }

    const officer = g.rank >= 2;
    const leader = g.rank >= 3;

    return `<h2>${esc(g.name)}</h2>
      ${g.motd ? `<p class="guild-motd">${esc(g.motd)}</p>` : ""}
      <div class="social-list">
        ${g.members
          .map(
            (m) => `<div class="social-row${m.online ? "" : " offline"}">
              <span class="social-name">${esc(m.name)}</span>
              <span class="social-sub">lv ${m.level} &middot; ${rankName(m.rank)}</span>
              ${
                leader && m.characterId !== "" && m.rank < g.rank
                  ? `<button class="social-mini" data-promote="${esc(m.name)}">&#9650;</button>
                     <button class="social-mini" data-demote="${esc(m.name)}">&#9660;</button>
                     <button class="social-mini" data-gkick="${esc(m.name)}">&times;</button>`
                  : ""
              }
            </div>`,
          )
          .join("")}
      </div>
      ${officer ? `<button class="social-action" data-guild="invite">Invite</button>` : ""}
      ${officer ? `<button class="social-action" data-guild="motd">Set message</button>` : ""}
      <button class="social-action" data-guild="leave">Leave guild</button>`;
  }

  #friendsSection(): string {
    const list = this.#friends?.friends ?? [];
    return `<h2>Friends</h2>
      ${
        list.length === 0
          ? `<p class="social-empty">Nobody yet.</p>`
          : `<div class="social-list">${list
              .map(
                (f) => `<div class="social-row${f.online ? "" : " offline"}">
                  <span class="social-name">${esc(f.name)}</span>
                  <span class="social-sub">${
                    f.online ? esc(f.mapId) : "offline"
                  } &middot; lv ${f.level}</span>
                  <button class="social-mini" data-unfriend="${esc(f.name)}">&times;</button>
                </div>`,
              )
              .join("")}</div>`
      }
      <button class="social-action" data-friend="add">Add a friend</button>`;
  }

  #bind(): void {
    const guildActions: Record<string, () => void> = {
      create: async () => {
        const name = await this.#cb.prompt("Name the guild:");
        if (name) this.#cb.onGuild(GuildAction_Kind.CREATE, name);
      },
      invite: async () => {
        const name = await this.#cb.prompt("Invite who?");
        if (name) this.#cb.onGuild(GuildAction_Kind.INVITE, name);
      },
      motd: async () => {
        const motd = await this.#cb.prompt("Message of the day:");
        if (motd !== null) this.#cb.onGuild(GuildAction_Kind.SET_MOTD, motd);
      },
      leave: () => this.#cb.onGuild(GuildAction_Kind.LEAVE),
    };

    for (const el of this.#root.querySelectorAll<HTMLButtonElement>("[data-guild]")) {
      el.addEventListener("click", () => guildActions[el.dataset.guild!]?.());
    }

    const bind = (attr: string, run: (value: string) => void) => {
      for (const el of this.#root.querySelectorAll<HTMLElement>(`[data-${attr}]`)) {
        el.addEventListener("click", () => run(el.dataset[attr]!));
      }
    };

    bind("promote", (n) => this.#cb.onGuild(GuildAction_Kind.PROMOTE, n));
    bind("demote", (n) => this.#cb.onGuild(GuildAction_Kind.DEMOTE, n));
    bind("gkick", (n) => this.#cb.onGuild(GuildAction_Kind.KICK, n));
    bind("unfriend", (n) => this.#cb.onFriends(SocialAction_Kind.REMOVE_FRIEND, n));

    for (const el of this.#root.querySelectorAll<HTMLButtonElement>("[data-friend]")) {
      el.addEventListener("click", async () => {
        const name = await this.#cb.prompt("Add who?");
        if (name) this.#cb.onFriends(SocialAction_Kind.ADD_FRIEND, name);
      });
    }
  }
}

function rankName(rank: number): string {
  if (rank >= 3) return "leader";
  if (rank >= 2) return "officer";
  return "member";
}

function esc(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}
