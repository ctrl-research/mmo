package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	gamedata "github.com/ctrl-research/mmo/content"
	"github.com/ctrl-research/mmo/internal/auth"
	"github.com/ctrl-research/mmo/internal/content"
	"github.com/ctrl-research/mmo/internal/rng"
	"github.com/ctrl-research/mmo/internal/store"
	"github.com/ctrl-research/mmo/internal/world"
	"github.com/ctrl-research/mmo/internal/world/items"
)

// Administration commands.
//
// The allowlist admits nobody by default, so a fresh server needs a way to let
// the first person in. Telling an operator to write SQL by hand is a bad
// answer: it is easy to get the match kind wrong, and getting it wrong on an
// allowlist fails open in the worst case.
//
//	mmo allow jonathan              add a local username
//	mmo allow alice bob carol       add several at once
//	mmo allow --provider=google --kind=email you@example.com
//	mmo allowlist                   list every rule
//	mmo revoke jonathan             remove a rule
//	mmo passwd jonathan             set a local account's password
//	mmo give Sigrun weapon.iron_sword --rarity=rare --ilvl=40
//	mmo mute Sigrun --for=24h --reason="advertising"
const adminUsage = `Usage:
  mmo allow [flags] VALUE...   allow one or more people to sign in
  mmo revoke [flags] VALUE...  remove one or more allowlist rules
  mmo allowlist                list allowlist rules
  mmo passwd USERNAME          set a local account's password
  mmo give CHARACTER BASE_ID   place an item in a character's inventory
  mmo mute CHARACTER           stop a character using chat
  mmo unmute CHARACTER         lift a mute

Flags for mute:
  --for        how long, e.g. 24h or 30m; omit for indefinite
  --reason     shown to the muted player, so a mute is not a silent failure

Flags for allow and revoke:
  --provider   identity provider, or empty for any (default: local)
  --kind       subject, email, or email_domain (default: subject)
  --note       a reminder of why the rule exists

Every command takes --database-url, or reads DATABASE_URL.
`

// runAdmin dispatches an administration subcommand, reporting whether one was
// recognised.
func runAdmin(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}

	switch args[0] {
	case "allow", "revoke", "allowlist", "passwd", "give", "mute", "unmute":
	default:
		return false, nil
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dbURL := fs.String("database-url",
		envOr("DATABASE_URL", "postgres://mmo:devpassword@localhost:5432/mmo?sslmode=disable"),
		"Postgres connection string")
	provider := fs.String("provider", "local", "identity provider, or empty for any")
	kind := fs.String("kind", store.MatchSubject, "subject, email, or email_domain")
	note := fs.String("note", "", "a reminder of why the rule exists")
	rarity := fs.String("rarity", "", "force a rarity: normal, magic, or rare")
	ilvl := fs.Int("ilvl", 0, "item level, deciding which affix tiers can roll")
	seed := fs.Uint64("seed", 0, "fixed roll seed, so a given item is reproducible")
	muteFor := fs.Duration("for", 0, "how long to mute for; zero means indefinite")
	reason := fs.String("reason", "", "why, shown to the muted player")
	fs.Usage = func() { fmt.Fprint(os.Stderr, adminUsage) }

	// Flags are separated from positional arguments before parsing, because
	// Go's flag package stops at the first non-flag argument -- so
	// "give Sigrun sword --rarity=rare" would silently ignore the flag and
	// produce a normal item, which looks like the generator being wrong.
	flags, positional := splitArgs(args[1:])
	if err := fs.Parse(flags); err != nil {
		return true, err
	}
	arg := func(i int) string {
		if i < len(positional) {
			return positional[i]
		}
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Quiet, so command output is the output rather than being buried under
	// migration logging.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	db, err := store.Open(ctx, store.Config{URL: *dbURL, Logger: log, MaxConns: 2})
	if err != nil {
		return true, err
	}
	defer db.Close()

	switch args[0] {
	case "allow":
		return true, adminAllow(ctx, db, positional, *provider, *kind, *note)
	case "revoke":
		return true, adminRevoke(ctx, db, positional, *provider, *kind)
	case "allowlist":
		return true, adminList(ctx, db)
	case "passwd":
		return true, adminPasswd(ctx, db, arg(0))
	case "give":
		return true, adminGive(ctx, db, arg(0), arg(1), *rarity, *ilvl, *seed)
	case "mute":
		return true, adminMute(ctx, db, arg(0), *muteFor, *reason)
	case "unmute":
		return true, adminUnmute(ctx, db, arg(0))
	}
	return true, nil
}

// adminAllow adds every value in one pass. Taking a list rather than a single
// value is what lets a deployment seed a whole allowlist in one invocation --
// the server image is distroless with no shell to loop with, so one value per
// process meant one container per user.
func adminAllow(ctx context.Context, db *store.Store, values []string, provider, kind, note string) error {
	if len(values) == 0 {
		return errors.New("a value is required: mmo allow USERNAME [USERNAME...]")
	}

	// Checked before anything is written, so a typo in the sixth argument
	// does not leave the first five applied and the command reporting failure.
	for i, value := range values {
		if value == "" {
			return fmt.Errorf("argument %d is empty; every value must be a username", i+1)
		}
	}

	scope := provider
	if scope == "" {
		scope = "any provider"
	}
	local := provider == "local" && kind == store.MatchSubject

	added := make([]string, 0, len(values))
	for _, value := range values {
		// Usernames are matched case-insensitively and stored normalised, so
		// an entry added as "Jonathan" must still match a sign-in as
		// "jonathan".
		if local {
			value = auth.NormaliseUsername(value)
		}

		// Named, because a bare constraint error gives no clue which of six
		// arguments was the bad one.
		if err := db.AddAllowlistEntry(ctx, provider, kind, value, note); err != nil {
			return fmt.Errorf("%s: %w", value, err)
		}
		fmt.Printf("allowed %s %q on %s\n", kind, value, scope)
		added = append(added, value)
	}

	if local {
		fmt.Printf("They can now register at /auth/local/register with: %s\n",
			strings.Join(added, ", "))
	}
	return nil
}

func adminRevoke(ctx context.Context, db *store.Store, values []string, provider, kind string) error {
	if len(values) == 0 {
		return errors.New("a value is required: mmo revoke USERNAME [USERNAME...]")
	}
	for _, value := range values {
		if err := revokeOne(ctx, db, value, provider, kind); err != nil {
			return fmt.Errorf("%s: %w", value, err)
		}
	}
	return nil
}

func revokeOne(ctx context.Context, db *store.Store, value, provider, kind string) error {
	if provider == "local" && kind == store.MatchSubject {
		value = auth.NormaliseUsername(value)
	}

	if err := db.RemoveAllowlistEntry(ctx, provider, kind, value); err != nil {
		return err
	}

	// Access ends at their next sign-in, because the allowlist is re-checked
	// every time rather than only at registration. An open session survives
	// until its access token expires.
	fmt.Printf("revoked %s %q\n", kind, value)
	fmt.Println("This takes effect at their next sign-in; any open session lasts until its token expires.")
	return nil
}

func adminList(ctx context.Context, db *store.Store) error {
	entries, err := db.ListAllowlist(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("The allowlist is empty, so nobody can sign in.")
		fmt.Println("Add someone with:  mmo allow USERNAME")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tKIND\tVALUE\tADDED\tNOTE")
	for _, e := range entries {
		provider := e.Provider
		if provider == "" {
			provider = "(any)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			provider, e.MatchKind, e.MatchValue, e.AddedAt.Format("2006-01-02"), e.Note)
	}
	return w.Flush()
}

func adminPasswd(ctx context.Context, db *store.Store, username string) error {
	if username == "" {
		return errors.New("a username is required: mmo passwd USERNAME")
	}
	normalised := auth.NormaliseUsername(username)

	cred, err := db.LocalCredentialByUsername(ctx, normalised)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no local account named %q", normalised)
	}
	if err != nil {
		return err
	}

	password, err := readPassword("New password: ")
	if err != nil {
		return err
	}
	again, err := readPassword("Again: ")
	if err != nil {
		return err
	}
	if password != again {
		return errors.New("passwords did not match")
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := db.UpdatePasswordHash(ctx, cred.AccountID, hash); err != nil {
		return err
	}

	// Clearing the lockout as well, since an operator resetting a password is
	// usually doing it because somebody is locked out.
	if err := db.ClearLoginFailures(ctx, cred.AccountID); err != nil {
		return err
	}

	fmt.Printf("password set for %q\n", cred.Username)
	return nil
}

// adminGive places an item in a character's inventory.
//
// For testing a drop without farming for it, and for a server owner making
// good after a genuine loss. It goes through the same generator and the same
// journal as a real drop, so what it produces is indistinguishable from one --
// and remains traceable in item_events as having come from here.
func adminGive(ctx context.Context, db *store.Store, characterName, baseID, rarity string, itemLevel int, seed uint64) error {
	if characterName == "" || baseID == "" {
		return errors.New("usage: mmo give CHARACTER BASE_ID")
	}

	game, err := content.Load(gamedata.FS)
	if err != nil {
		return fmt.Errorf("loading content: %w", err)
	}
	if _, ok := game.Items[baseID]; !ok {
		return fmt.Errorf("no item base named %q", baseID)
	}

	character, err := db.CharacterByName(ctx, characterName)
	if err != nil {
		return err
	}

	if itemLevel <= 0 {
		itemLevel = character.Level
	}
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}

	weights := items.DefaultRarityWeights
	switch rarity {
	case "normal":
		weights = items.RarityWeights{Normal: 1}
	case "magic":
		weights = items.RarityWeights{Magic: 1}
	case "rare":
		weights = items.RarityWeights{Rare: 1}
	case "":
	default:
		return fmt.Errorf("unknown rarity %q, want normal, magic, or rare", rarity)
	}

	gen := items.NewGenerator(game)
	inst, err := gen.Roll(rng.New(seed), baseID, itemLevel, weights)
	if err != nil {
		return err
	}

	inventory, _, err := db.EnsureContainers(ctx, character.ID, world.InventorySlots, world.EquipmentSlots)
	if err != nil {
		return err
	}

	slot, err := db.FreeSlot(ctx, inventory.ID)
	if errors.Is(err, store.ErrContainerFull) {
		return fmt.Errorf("%s's inventory is full", character.Name)
	}
	if err != nil {
		return err
	}

	mods, err := json.Marshal(inst)
	if err != nil {
		return err
	}

	if _, err := db.InsertItem(ctx, inventory.ID, slot, store.ItemRow{
		BaseID:    inst.BaseID,
		Rarity:    string(inst.Rarity),
		ItemLevel: inst.ItemLevel,
		Mods:      mods,
		StackSize: inst.Stack,
	}, character.ID, store.EventCreate, 0); err != nil {
		return err
	}

	fmt.Printf("gave %s: %s (%s, item level %d)\n",
		character.Name, gen.DisplayName(inst), inst.Rarity, inst.ItemLevel)
	for _, m := range inst.Implicits {
		fmt.Printf("  %s %s %v\n", m.Stat, m.Kind, m.Value)
	}
	for _, m := range inst.Affixes {
		fmt.Printf("  %s %s %v (T%d)\n", m.Stat, m.Kind, m.Value, m.Tier)
	}

	if character.LeaseToken > 0 {
		fmt.Println("\nThis character may be in play; they will see it after reconnecting.")
	}
	return nil
}

// splitArgs separates flags from positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so a flag
// written after a positional one is silently ignored -- and a silently ignored
// --rarity looks like the item generator being wrong rather than like an
// argument that never arrived.
//
// A flag taking a separate value ("--rarity rare" rather than "--rarity=rare")
// consumes the next argument, which is why the known set is checked here.
func splitArgs(args []string) (flags, positional []string) {
	takesValue := map[string]bool{
		"-provider": true, "--provider": true,
		"-kind": true, "--kind": true,
		"-note": true, "--note": true,
		"-rarity": true, "--rarity": true,
		"-ilvl": true, "--ilvl": true,
		"-seed": true, "--seed": true,
		"-database-url": true, "--database-url": true,
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) == 0 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}

		flags = append(flags, a)

		// "--flag=value" carries its own value; "--flag value" takes the next.
		if !strings.Contains(a, "=") && takesValue[a] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

// adminMute stops a character using chat.
func adminMute(ctx context.Context, db *store.Store, characterName string,
	within time.Duration, reason string,
) error {
	if characterName == "" {
		return errors.New("mute: give a character name")
	}

	c, err := db.CharacterByName(ctx, characterName)
	if err != nil {
		return err
	}

	var expires *time.Time
	if within > 0 {
		at := time.Now().Add(within)
		expires = &at
	}

	if err := db.MuteCharacter(ctx, c.ID, expires, reason, "cli"); err != nil {
		return err
	}

	until := "indefinitely"
	if expires != nil {
		until = "until " + expires.Format(time.RFC3339)
	}
	fmt.Printf("muted %s %s\n", c.Name, until)
	if reason != "" {
		fmt.Printf("reason: %s\n", reason)
	}
	return nil
}

// adminUnmute lifts a mute.
func adminUnmute(ctx context.Context, db *store.Store, characterName string) error {
	if characterName == "" {
		return errors.New("unmute: give a character name")
	}

	c, err := db.CharacterByName(ctx, characterName)
	if err != nil {
		return err
	}
	if err := db.UnmuteCharacter(ctx, c.ID, "cli"); err != nil {
		return err
	}

	fmt.Printf("unmuted %s\n", c.Name)
	return nil
}
