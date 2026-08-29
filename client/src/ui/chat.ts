import { ChatChannel } from "@/net/connection";
import type { ChatLine, SystemMessage } from "@/net/connection";

/**
 * The chat window.
 *
 * Five channels that differ in who hears you, shown in one scrollback with a
 * colour each rather than five tabs. Tabs hide the thing chat is for: the
 * message you were not looking at. Colour keeps everything visible and still
 * tells you at a glance which channel a line came from.
 *
 * The input is the only place in the game that takes free text, so it is also
 * the only place that can steal the movement keys. It grabs focus on Enter and
 * gives it back on Enter or Escape, and nothing is typed into the world while
 * it holds it.
 */

export interface ChatCallbacks {
  onSend(channel: ChatChannel, body: string, target: string): void;

  /** Called when the input takes or releases the keyboard. */
  onFocusChange(focused: boolean): void;
}

/** How many lines to keep. Enough to scroll back through a fight. */
const SCROLLBACK = 200;

interface Entry {
  channel: ChatChannel;
  from: string;
  body: string;
  outgoing: boolean;
  system: boolean;
}

/** The channel a bare message goes to, and what the prefixes switch it to. */
const PREFIXES: Array<[string, ChatChannel]> = [
  ["/g ", ChatChannel.GLOBAL],
  ["/p ", ChatChannel.PARTY],
  ["/gu ", ChatChannel.GUILD],
  ["/s ", ChatChannel.LOCAL],
];

const CHANNEL_CLASS: Record<number, string> = {
  [ChatChannel.LOCAL]: "chat-local",
  [ChatChannel.GLOBAL]: "chat-global",
  [ChatChannel.WHISPER]: "chat-whisper",
  [ChatChannel.PARTY]: "chat-party",
  [ChatChannel.GUILD]: "chat-guild",
};

const CHANNEL_TAG: Record<number, string> = {
  [ChatChannel.LOCAL]: "",
  [ChatChannel.GLOBAL]: "[global] ",
  [ChatChannel.WHISPER]: "",
  [ChatChannel.PARTY]: "[party] ",
  [ChatChannel.GUILD]: "[guild] ",
};

export class ChatPanel {
  #log: HTMLElement;
  #input: HTMLInputElement;
  #cb: ChatCallbacks;

  #entries: Entry[] = [];

  /** The channel a plain message goes to, remembered between messages. */
  #channel: ChatChannel = ChatChannel.LOCAL;

  /** Who a bare reply goes to, set by the last whisper either way. */
  #replyTo = "";

  constructor(root: HTMLElement, cb: ChatCallbacks) {
    this.#cb = cb;

    root.innerHTML = `
      <div class="chat-log"></div>
      <input class="chat-input" type="text" maxlength="300"
             placeholder="enter to chat &middot; /g global &middot; /p party &middot; /gu guild &middot; /w name" />`;

    this.#log = root.querySelector(".chat-log")!;
    this.#input = root.querySelector(".chat-input")!;

    this.#input.addEventListener("keydown", (e) => this.#onKey(e));
    this.#input.addEventListener("blur", () => this.#cb.onFocusChange(false));
    this.#input.addEventListener("focus", () => this.#cb.onFocusChange(true));
  }

  get focused(): boolean {
    return document.activeElement === this.#input;
  }

  /** Takes the keyboard, so the next thing typed is a message. */
  focus(): void {
    this.#input.focus();
  }

  blur(): void {
    this.#input.blur();
  }

  /** Adds a line somebody said. */
  add(line: ChatLine): void {
    // A whisper either way sets the reply target, so answering one is Enter
    // and typing rather than retyping a name.
    if (line.channel === ChatChannel.WHISPER) this.#replyTo = line.from;

    this.#push({
      channel: line.channel,
      from: line.from,
      body: line.body,
      outgoing: line.outgoing,
      system: false,
    });
  }

  /** Adds a notice from the server. */
  addSystem(msg: SystemMessage): void {
    this.#push({
      channel: msg.channel,
      from: "",
      body: msg.body,
      outgoing: false,
      system: true,
    });
  }

  /** Adds a notice the client itself produced. */
  note(body: string): void {
    this.#push({
      channel: ChatChannel.UNSPECIFIED,
      from: "",
      body,
      outgoing: false,
      system: true,
    });
  }

  #push(entry: Entry): void {
    this.#entries.push(entry);
    if (this.#entries.length > SCROLLBACK) this.#entries.shift();
    this.#render();
  }

  #render(): void {
    // Pinned to the bottom unless the player has scrolled up to read
    // something, which is exactly when yanking them back down is worst.
    const atBottom =
      this.#log.scrollHeight - this.#log.scrollTop - this.#log.clientHeight < 40;

    this.#log.innerHTML = this.#entries.map(renderEntry).join("");
    if (atBottom) this.#log.scrollTop = this.#log.scrollHeight;
  }

  #onKey(e: KeyboardEvent): void {
    // Every key typed here is text, not movement. Without this the character
    // runs off while its player writes a sentence.
    e.stopPropagation();

    if (e.key === "Escape") {
      this.#input.value = "";
      this.#input.blur();
      return;
    }
    if (e.key !== "Enter") return;

    const raw = this.#input.value.trim();
    this.#input.value = "";
    if (raw === "") {
      this.#input.blur();
      return;
    }

    this.#submit(raw);
  }

  /**
   * Turns what was typed into a channel, a target, and a message.
   *
   * The prefixes are the whole interface: a slash command that switches
   * channel for one message is faster than a channel selector, and switching
   * for good is what typing the prefix with nothing after it does.
   */
  #submit(raw: string): void {
    // "/w name message" and "/r message" are the two shapes a whisper takes.
    const whisper = /^\/w\s+(\S+)\s+([\s\S]+)$/.exec(raw);
    if (whisper) {
      this.#replyTo = whisper[1]!;
      this.#cb.onSend(ChatChannel.WHISPER, whisper[2]!, whisper[1]!);
      return;
    }

    const reply = /^\/r\s+([\s\S]+)$/.exec(raw);
    if (reply) {
      if (this.#replyTo === "") {
        this.note("nobody has whispered you yet");
        return;
      }
      this.#cb.onSend(ChatChannel.WHISPER, reply[1]!, this.#replyTo);
      return;
    }

    for (const [prefix, channel] of PREFIXES) {
      if (raw === prefix.trim()) {
        // The prefix on its own switches channel for everything after it.
        this.#channel = channel;
        this.note(`now talking in ${channelName(channel)}`);
        return;
      }
      if (raw.startsWith(prefix)) {
        this.#cb.onSend(channel, raw.slice(prefix.length), "");
        return;
      }
    }

    if (raw.startsWith("/")) {
      this.note(`unknown command ${raw.split(/\s/)[0]}`);
      return;
    }

    this.#cb.onSend(this.#channel, raw, "");
  }
}

function renderEntry(e: Entry): string {
  if (e.system) {
    return `<div class="chat-line chat-system">${esc(e.body)}</div>`;
  }

  const cls = CHANNEL_CLASS[e.channel] ?? "chat-local";
  const tag = CHANNEL_TAG[e.channel] ?? "";

  // A whisper you sent reads "to Alice", one you received reads "Alice
  // whispers". Without the distinction the two halves of a conversation look
  // identical and it is impossible to tell who said what.
  let who = `${esc(e.from)}: `;
  if (e.channel === ChatChannel.WHISPER) {
    who = e.outgoing ? `to ${esc(e.from)}: ` : `${esc(e.from)} whispers: `;
  }

  return `<div class="chat-line ${cls}"><span class="chat-who">${tag}${who}</span>${esc(e.body)}</div>`;
}

function channelName(channel: ChatChannel): string {
  switch (channel) {
    case ChatChannel.GLOBAL:
      return "global";
    case ChatChannel.PARTY:
      return "party";
    case ChatChannel.GUILD:
      return "guild";
    default:
      return "local";
  }
}

function esc(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!,
  );
}
