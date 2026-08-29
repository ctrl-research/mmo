/**
 * The pre-game HTTP API: signing in, and choosing a character.
 *
 * Everything here happens before a socket exists. Identity and character
 * selection are settled over authenticated HTTP, and the game connection then
 * carries only a single-use ticket -- so the game protocol never has to know
 * who anyone is.
 */

export interface Provider {
  id: string;
  displayName: string;
}

export interface Character {
  id: string;
  name: string;
  class: string;
  level: number;
  exp: number;
  gold: number;
  mapId: string;
}

/** Raised for a response the caller is expected to show the player. */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/**
 * request wraps fetch with the two things every call here needs: cookies, and
 * an error that carries the server's own message.
 *
 * Sessions live in HttpOnly cookies, so script cannot read them -- which is
 * the point -- and every request must therefore opt into sending them.
 */
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });

  const body = await res.text();

  if (!res.ok) {
    // The server sends a JSON error with a message written for a person.
    // Falling back to the status text loses that, so only do so when the body
    // is not what we expect.
    let message = res.statusText;
    try {
      const parsed = JSON.parse(body) as { error?: string };
      if (parsed.error) message = parsed.error;
    } catch {
      if (body.trimStart().startsWith("<")) {
        message = "This address is not the game server.";
      }
    }
    throw new ApiError(res.status, message);
  }

  return body ? (JSON.parse(body) as T) : ({} as T);
}

export async function loadProviders(): Promise<{
  providers: Provider[];
  devAuth: boolean;
  localAuth: boolean;
}> {
  return request("/auth/providers");
}

export async function localLogin(username: string, password: string): Promise<void> {
  await request("/auth/local/login", {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
}

export async function localRegister(username: string, password: string): Promise<void> {
  await request("/auth/local/register", {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
}

export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  await request("/api/password", {
    method: "POST",
    body: JSON.stringify({ currentPassword, newPassword }),
  });
}

/** Reports whether a session is already established. */
export async function currentAccount(): Promise<string | null> {
  try {
    const me = await request<{ accountId: string }>("/api/me");
    return me.accountId;
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return null;
    throw err;
  }
}

/**
 * Refreshes an expired access token.
 *
 * Access tokens are deliberately short-lived, so this is the normal path
 * rather than an error case: a player who leaves the tab open comes back to an
 * expired token and a valid refresh cookie.
 */
export async function refreshSession(): Promise<boolean> {
  try {
    await request("/auth/refresh", { method: "POST" });
    return true;
  } catch {
    return false;
  }
}

export async function devLogin(subject: string): Promise<void> {
  await request("/auth/dev/login", {
    method: "POST",
    body: JSON.stringify({ subject }),
  });
}

export async function logout(): Promise<void> {
  await request("/auth/logout", { method: "POST" });
}

export async function listCharacters(): Promise<{ characters: Character[]; max: number }> {
  return request("/api/characters");
}

/** One playable class, as the character screen shows it. */
export interface ClassInfo {
  id: string;
  name: string;
  description: string;
  primaryStat: string;
}

/**
 * The playable classes.
 *
 * From the server rather than a list in the client: classes are content, and a
 * client with its own copy would offer one the server does not have -- which
 * produces a character with no skills and no way to act.
 */
export async function listClasses(): Promise<{ classes: ClassInfo[] }> {
  return request("/api/classes");
}

export async function createCharacter(name: string, cls: string): Promise<Character> {
  return request("/api/characters", {
    method: "POST",
    body: JSON.stringify({ name, class: cls }),
  });
}

export async function deleteCharacter(id: string): Promise<void> {
  await request(`/api/characters/${id}`, { method: "DELETE" });
}

/** Requests a single-use ticket for one character. */
export async function requestTicket(characterId: string): Promise<string> {
  const res = await request<{ ticket: string }>("/api/ticket", {
    method: "POST",
    body: JSON.stringify({ characterId }),
  });
  return res.ticket;
}

/**
 * Confirms this origin is really the game server before anything else.
 *
 * In development the requests go through the Vite proxy, and if its target is
 * wrong or occupied, that something else answers happily. Checking here turns
 * an unreadable parse error into a message that names the problem.
 */
export async function serverInfo(): Promise<{ protocol: number; content: string }> {
  const res = await fetch("/healthz");
  const body = await res.text();

  let info: { protocol?: number; content?: string };
  try {
    info = JSON.parse(body) as { protocol?: number; content?: string };
  } catch {
    throw new Error(wrongServer(body));
  }
  if (typeof info.protocol !== "number") {
    throw new Error(wrongServer(body));
  }
  return { protocol: info.protocol, content: info.content ?? "" };
}

function wrongServer(body: string): string {
  const html = body.trimStart().startsWith("<");
  return (
    "This address is not the game server" +
    (html ? " -- it returned an HTML page" : "") +
    ". If you are using the Vite dev server, check that the game server is " +
    "running and that MMO_SERVER points at its port, for example: " +
    "MMO_SERVER=http://localhost:8088 npm run dev"
  );
}
