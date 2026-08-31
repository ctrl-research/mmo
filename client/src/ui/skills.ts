import type { SecondarySkill, SecondaryExp } from "@/net/connection";

/**
 * The skills panel: every secondary skill, its level, and how far into it.
 *
 * Secondary skills are the one part of progression with no presence in the
 * world — a character's main level is on their frame and their gear is on their
 * body, but "my woodcutting is 34" exists nowhere unless there is somewhere to
 * read it. That makes the panel part of the feature rather than a convenience.
 *
 * Levels are derived on the server and sent, not computed here. The OSRS curve
 * is a 99-entry table; a copy in the client is a copy that disagrees with the
 * server the first time either is touched.
 */

/** How long a level-up stays lit on its row. */
const CELEBRATE_MS = 6_000;

interface Row {
  root: HTMLElement;
  level: HTMLElement;
  fill: HTMLElement;
  detail: HTMLElement;
  tool: HTMLElement;
  litAt: number;
}

export class SkillsPanel {
  #root: HTMLElement;
  #body: HTMLElement;
  #rows = new Map<string, Row>();

  /** The last full state, so a single gain can be applied on top of it. */
  #state = new Map<string, SecondarySkill>();

  constructor(root: HTMLElement) {
    this.#root = root;
    root.innerHTML =
      '<div class="skills-header">' +
      "<span>Skills</span>" +
      '<span class="skills-hint">hold E at a tree, rock, or patch · J to close</span>' +
      "</div>" +
      '<div class="skills-body"></div>';

    this.#body = root.querySelector(".skills-body") as HTMLElement;
    root.hidden = true;
  }

  get isOpen(): boolean {
    return !this.#root.hidden;
  }

  toggle(): void {
    this.#root.hidden = !this.#root.hidden;
  }

  close(): void {
    this.#root.hidden = true;
  }

  /** The whole set, sent on entering the world and when equipment changes. */
  setAll(skills: SecondarySkill[]): void {
    for (const s of skills) this.#state.set(s.skill, s);
    for (const s of skills) this.#apply(s, false);
  }

  /**
   * One gain, applied on top of the last full state.
   *
   * A gain carries the level and the next threshold but not the skill's name or
   * its tool class, because those never change — resending them per log would
   * be a string on the wire every few seconds for something the client already
   * knows.
   */
  gained(exp: SecondaryExp, now: number): void {
    const prev = this.#state.get(exp.skill);
    if (!prev) return;

    const next: SecondarySkill = {
      ...prev,
      total: exp.total,
      level: exp.level,
      levelAt: exp.levelAt,
      nextAt: exp.nextAt,
    };
    this.#state.set(exp.skill, next);
    this.#apply(next, exp.levelUp, now);
  }

  /** Called every frame: fades a level-up highlight. */
  render(now: number): void {
    for (const row of this.#rows.values()) {
      if (row.litAt === 0) continue;
      if (now - row.litAt >= CELEBRATE_MS) {
        row.litAt = 0;
        row.root.classList.remove("levelled");
      }
    }
  }

  #apply(s: SecondarySkill, levelled: boolean, now = 0): void {
    const row = this.#rowFor(s);

    row.level.textContent = String(s.level);

    // Progress *through* the current level, which is why the server sends both
    // ends of it rather than only the total. Measured from zero the bar would
    // sit near full for the whole late game -- true of the total, and useless
    // as a bar.
    const from = Number(s.levelAt);
    const span = Number(s.nextAt) - from;
    const into = Number(s.total) - from;
    const maxed = s.nextAt === 0n || span <= 0;

    row.fill.style.width = maxed
      ? "100%"
      : `${Math.max(0, Math.min(100, (into / span) * 100)).toFixed(1)}%`;

    row.detail.textContent = maxed
      ? `${s.total} xp — maxed`
      : `${s.total} / ${s.nextAt} xp`;

    // What is in hand, because "nothing happens when I press E" is otherwise
    // unanswerable from inside the game.
    if (!s.toolName) {
      row.tool.textContent = "no tool needed";
      row.tool.className = "skills-tool bare";
    } else if (s.toolPower > 0) {
      row.tool.textContent = `${s.toolName} · power ${s.toolPower}`;
      row.tool.className = "skills-tool held";
    } else {
      row.tool.textContent = `needs ${aOrAn(s.toolName)}`;
      row.tool.className = "skills-tool missing";
    }

    if (levelled) {
      row.litAt = now;
      row.root.classList.add("levelled");
    }
  }

  #rowFor(s: SecondarySkill): Row {
    const existing = this.#rows.get(s.skill);
    if (existing) return existing;

    const root = document.createElement("div");
    root.className = "skills-row";
    root.innerHTML =
      '<div class="skills-level"></div>' +
      '<div class="skills-main">' +
      `<div class="skills-name">${escapeText(s.name)}</div>` +
      '<div class="skills-bar"><div class="skills-fill"></div></div>' +
      '<div class="skills-detail"></div>' +
      "</div>" +
      '<div class="skills-tool"></div>';
    this.#body.append(root);

    const row: Row = {
      root,
      level: root.querySelector(".skills-level") as HTMLElement,
      fill: root.querySelector(".skills-fill") as HTMLElement,
      detail: root.querySelector(".skills-detail") as HTMLElement,
      tool: root.querySelector(".skills-tool") as HTMLElement,
      litAt: 0,
    };
    this.#rows.set(s.skill, row);
    return row;
  }
}

function aOrAn(word: string): string {
  return /^[aeiou]/i.test(word) ? `an ${word}` : `a ${word}`;
}

function escapeText(s: string): string {
  const el = document.createElement("div");
  el.textContent = s;
  return el.innerHTML;
}
