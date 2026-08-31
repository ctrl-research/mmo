package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Item persistence.
//
// The invariants live in the schema rather than here, which is deliberate: a
// rule enforced in Go is a rule some future code path forgets, and item
// duplication is the bug that costs an economy. This file's job is to keep
// every mutation inside a transaction that also writes the journal.

// Item errors.
var (
	// ErrSlotOccupied means another item already holds the target slot.
	ErrSlotOccupied = errors.New("store: slot is occupied")

	// ErrContainerFull means there is no free slot.
	ErrContainerFull = errors.New("store: container is full")
)

// Container kinds.
const (
	ContainerInventory = "inventory"
	ContainerEquipment = "equipment"
	ContainerBank      = "bank"
	ContainerTrade     = "trade"
	ContainerMail      = "mail"
)

// Owner types.
const OwnerCharacter = "character"

// Item event kinds, recorded in the journal.
const (
	EventCreate  = "create"
	EventPickup  = "pickup"
	EventMove    = "move"
	EventEquip   = "equip"
	EventUnequip = "unequip"
	EventDrop    = "drop"
	EventVendor  = "vendor"
	EventDestroy = "destroy"
)

// Container is a place items live.
type Container struct {
	ID        uuid.UUID
	Kind      string
	OwnerType string
	OwnerID   uuid.UUID
	Capacity  int
}

// ItemRow is a persisted item instance.
type ItemRow struct {
	ID          uuid.UUID
	BaseID      string
	Rarity      string
	ItemLevel   int
	Mods        json.RawMessage
	StackSize   int
	ContainerID uuid.UUID
	Slot        int
	CreatedAt   time.Time
}

// EnsureContainers creates a character's inventory and equipment if they do
// not exist, and returns them.
//
// Idempotent, so it can be called on every login without a prior check.
func (s *Store) EnsureContainers(ctx context.Context, characterID uuid.UUID, inventorySlots, equipmentSlots int) (inventory, equipment Container, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		inventory, err = ensureContainer(ctx, tx, characterID, ContainerInventory, inventorySlots)
		if err != nil {
			return err
		}
		equipment, err = ensureContainer(ctx, tx, characterID, ContainerEquipment, equipmentSlots)
		return err
	})
	if err != nil {
		return Container{}, Container{}, fmt.Errorf("store: ensuring containers: %w", err)
	}
	return inventory, equipment, nil
}

func ensureContainer(ctx context.Context, tx pgx.Tx, ownerID uuid.UUID, kind string, capacity int) (Container, error) {
	var c Container
	// ON CONFLICT rather than a prior SELECT: two logins racing would both see
	// no container and both try to create one.
	err := tx.QueryRow(ctx, `
		INSERT INTO containers (kind, owner_type, owner_id, capacity)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner_type, owner_id, kind)
		DO UPDATE SET capacity = GREATEST(containers.capacity, EXCLUDED.capacity)
		RETURNING id, kind::text, owner_type, owner_id, capacity`,
		kind, OwnerCharacter, ownerID, capacity,
	).Scan(&c.ID, &c.Kind, &c.OwnerType, &c.OwnerID, &c.Capacity)
	return c, err
}

// InsertItem places a new item into a container, recording its creation.
//
// The item and its journal entry are written in one transaction: an item that
// exists with no record of where it came from is exactly what makes a
// duplication investigation impossible.
func (s *Store) InsertItem(ctx context.Context, containerID uuid.UUID, slot int, item ItemRow, actor uuid.UUID, kind string, tick uint64) (uuid.UUID, error) {
	var id uuid.UUID

	mods := item.Mods
	if len(mods) == 0 {
		mods = json.RawMessage("{}")
	}
	stack := item.StackSize
	if stack < 1 {
		stack = 1
	}

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO item_instances
			    (base_id, rarity, item_level, mods, stack_size, container_id, slot)
			VALUES ($1, $2::item_rarity, $3, $4, $5, $6, $7)
			RETURNING id`,
			item.BaseID, item.Rarity, item.ItemLevel, mods, stack, containerID, slot,
		).Scan(&id)
		if err != nil {
			return err
		}
		return recordEvent(ctx, tx, id, kind, nil, nil, &containerID, &slot, actor, tick)
	})

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return uuid.Nil, ErrSlotOccupied
		}
		return uuid.Nil, fmt.Errorf("store: inserting item: %w", err)
	}
	return id, nil
}

// StackInto adds a quantity to an existing stack of the same base item, and
// reports the item it landed in.
//
// The zero UUID with no error means there was no stack with room, and the
// caller should insert a new one. Not an error: "nothing to merge into" is the
// ordinary case for the first of anything.
//
// One statement, because two concurrent grants that both read a stack size and
// both write the same total is the arithmetic version of destroying an item.
// Two things make that unrepresentable here, and both are properties of the
// statement rather than of anything this function does:
//
//   - `stack_size + $3` is evaluated by the UPDATE against the row it has
//     locked, so increments serialise instead of racing;
//   - when the UPDATE finds its target concurrently modified, READ COMMITTED
//     re-runs the whole qualification from the newer snapshot -- the sub-select
//     included. So a second grant onto a stack that has just reached its
//     maximum re-runs the sub-select, finds nothing with room, and updates
//     nothing, returning the zero UUID and sending the caller to a fresh stack.
//
// The second point is worth stating because it is not obvious and it is what
// keeps the cap honest. Writing the cap a second time in the UPDATE's own WHERE
// looked like the load-bearing version of the check; it was redundant, and no
// amount of contention could make a test fail without it. The two concurrency
// tests in items_test.go are what actually hold this line.
func (s *Store) StackInto(
	ctx context.Context,
	containerID uuid.UUID,
	baseID string,
	qty, maxStack int,
	actor uuid.UUID,
	tick uint64,
) (uuid.UUID, int, error) {
	if qty < 1 || maxStack < 2 {
		return uuid.Nil, 0, nil
	}

	var (
		id    uuid.UUID
		stack int
		slot  int
	)

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lowest slot first, so a bag fills predictably rather than in whatever
		// order the rows happen to come back in.
		err := tx.QueryRow(ctx, `
			UPDATE item_instances
			   SET stack_size = stack_size + $3, updated_at = now()
			 WHERE id = (
			       SELECT id FROM item_instances
			        WHERE container_id = $1
			          AND base_id = $2
			          AND stack_size + $3 <= $4
			        ORDER BY slot
			        LIMIT 1)
			RETURNING id, stack_size, slot`,
			containerID, baseID, qty, maxStack,
		).Scan(&id, &stack, &slot)
		if errors.Is(err, pgx.ErrNoRows) {
			// No stack with room. Not an error, and not something to record.
			return nil
		}
		if err != nil {
			return err
		}
		// Journalled like any other movement of quantity: a stack growing is a
		// thing that happened to an item, and an investigation that could not
		// see it would be an investigation with a gap in it.
		return recordEvent(ctx, tx, id, EventPickup, nil, nil, &containerID, &slot, actor, tick)
	})
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("store: stacking item: %w", err)
	}
	return id, stack, nil
}

// MoveItem relocates an item, recording the move.
//
// An UPDATE of the location, never a delete and an insert: a delete-then-insert
// that fails between the two destroys an item, and a retry of the insert alone
// duplicates one.
func (s *Store) MoveItem(ctx context.Context, itemID, toContainer uuid.UUID, toSlot int, actor uuid.UUID, kind string, tick uint64) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var fromContainer uuid.UUID
		var fromSlot int

		// Locked for the duration, so two simultaneous moves of one item
		// serialise rather than both succeeding against a stale read.
		err := tx.QueryRow(ctx,
			`SELECT container_id, slot FROM item_instances WHERE id = $1 FOR UPDATE`,
			itemID).Scan(&fromContainer, &fromSlot)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE item_instances
			   SET container_id = $2, slot = $3, updated_at = now()
			 WHERE id = $1`,
			itemID, toContainer, toSlot); err != nil {
			return err
		}

		return recordEvent(ctx, tx, itemID, kind,
			&fromContainer, &fromSlot, &toContainer, &toSlot, actor, tick)
	})

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrSlotOccupied
		}
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("store: moving item: %w", err)
	}
	return nil
}

// SwapItems exchanges the locations of two items in one transaction.
//
// Needed because the unique constraint makes a two-step swap impossible: the
// first move would collide with the item still occupying the target. Moving
// one aside first would leave a window where an interrupted swap loses track
// of where something belongs.
func (s *Store) SwapItems(ctx context.Context, firstID, secondID uuid.UUID, actor uuid.UUID, tick uint64) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		type loc struct {
			container uuid.UUID
			slot      int
		}
		var a, b loc

		// Locked in a deterministic order, so two swaps involving the same
		// pair cannot deadlock against each other.
		firstLock, secondLock := firstID, secondID
		if secondLock.String() < firstLock.String() {
			firstLock, secondLock = secondLock, firstLock
		}
		for _, id := range []uuid.UUID{firstLock, secondLock} {
			if _, err := tx.Exec(ctx,
				`SELECT 1 FROM item_instances WHERE id = $1 FOR UPDATE`, id); err != nil {
				return err
			}
		}

		if err := tx.QueryRow(ctx,
			`SELECT container_id, slot FROM item_instances WHERE id = $1`, firstID,
		).Scan(&a.container, &a.slot); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT container_id, slot FROM item_instances WHERE id = $1`, secondID,
		).Scan(&b.container, &b.slot); err != nil {
			return err
		}

		// Uniqueness is checked at commit rather than per statement, so the
		// two updates can cross without the first colliding with the item the
		// second is about to move. The constraint still holds -- a swap that
		// would leave two items in one slot fails at commit.
		if _, err := tx.Exec(ctx,
			`SET CONSTRAINTS item_instances_location_unique DEFERRED`); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE item_instances SET container_id = $2, slot = $3, updated_at = now() WHERE id = $1`,
			firstID, b.container, b.slot); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE item_instances SET container_id = $2, slot = $3, updated_at = now() WHERE id = $1`,
			secondID, a.container, a.slot); err != nil {
			return err
		}

		if err := recordEvent(ctx, tx, firstID, EventMove,
			&a.container, &a.slot, &b.container, &b.slot, actor, tick); err != nil {
			return err
		}
		return recordEvent(ctx, tx, secondID, EventMove,
			&b.container, &b.slot, &a.container, &a.slot, actor, tick)
	})

	if err != nil {
		return fmt.Errorf("store: swapping items: %w", err)
	}
	return nil
}

// DestroyItem removes an item, recording its destruction.
func (s *Store) DestroyItem(ctx context.Context, itemID uuid.UUID, actor uuid.UUID, tick uint64) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var fromContainer uuid.UUID
		var fromSlot int

		err := tx.QueryRow(ctx,
			`SELECT container_id, slot FROM item_instances WHERE id = $1 FOR UPDATE`,
			itemID).Scan(&fromContainer, &fromSlot)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		// The journal entry is written before the row goes, so the record of
		// destruction survives the thing it describes. item_events
		// deliberately has no foreign key for the same reason.
		if err := recordEvent(ctx, tx, itemID, EventDestroy,
			&fromContainer, &fromSlot, nil, nil, actor, tick); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `DELETE FROM item_instances WHERE id = $1`, itemID)
		return err
	})

	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("store: destroying item: %w", err)
	}
	return nil
}

// LoadContainer returns every item in a container, ordered by slot.
func (s *Store) LoadContainer(ctx context.Context, containerID uuid.UUID) ([]ItemRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, base_id, rarity::text, item_level, mods, stack_size, container_id, slot, created_at
		  FROM item_instances
		 WHERE container_id = $1
		 ORDER BY slot`, containerID)
	if err != nil {
		return nil, fmt.Errorf("store: loading container: %w", err)
	}
	defer rows.Close()

	var out []ItemRow
	for rows.Next() {
		var it ItemRow
		if err := rows.Scan(&it.ID, &it.BaseID, &it.Rarity, &it.ItemLevel, &it.Mods,
			&it.StackSize, &it.ContainerID, &it.Slot, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// LoadItem returns one item.
func (s *Store) LoadItem(ctx context.Context, itemID uuid.UUID) (ItemRow, error) {
	var it ItemRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, base_id, rarity::text, item_level, mods, stack_size, container_id, slot, created_at
		  FROM item_instances WHERE id = $1`, itemID,
	).Scan(&it.ID, &it.BaseID, &it.Rarity, &it.ItemLevel, &it.Mods,
		&it.StackSize, &it.ContainerID, &it.Slot, &it.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return ItemRow{}, ErrNotFound
	}
	if err != nil {
		return ItemRow{}, fmt.Errorf("store: loading item: %w", err)
	}
	return it, nil
}

// FreeSlot returns the lowest unoccupied slot in a container.
func (s *Store) FreeSlot(ctx context.Context, containerID uuid.UUID) (int, error) {
	var capacity int
	if err := s.pool.QueryRow(ctx,
		`SELECT capacity FROM containers WHERE id = $1`, containerID).Scan(&capacity); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("store: reading container: %w", err)
	}

	// generate_series minus the occupied slots: the database finds the gap in
	// one round trip rather than the caller fetching every slot to scan it.
	var slot int
	err := s.pool.QueryRow(ctx, `
		SELECT s FROM generate_series(0, $2 - 1) AS s
		 WHERE NOT EXISTS (
		       SELECT 1 FROM item_instances
		        WHERE container_id = $1 AND slot = s)
		 ORDER BY s LIMIT 1`, containerID, capacity).Scan(&slot)

	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrContainerFull
	}
	if err != nil {
		return 0, fmt.Errorf("store: finding a free slot: %w", err)
	}
	return slot, nil
}

// ItemEvent is one entry from the journal.
type ItemEvent struct {
	ID     int64
	ItemID uuid.UUID
	Kind   string
	At     time.Time
}

// ItemHistory returns an item's journal, oldest first.
//
// This is what makes a duplication investigation possible: an item with two
// concurrent moves into different containers is a dupe, visible here and
// nowhere else.
func (s *Store) ItemHistory(ctx context.Context, itemID uuid.UUID) ([]ItemEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, item_id, kind, at FROM item_events WHERE item_id = $1 ORDER BY id`, itemID)
	if err != nil {
		return nil, fmt.Errorf("store: reading item history: %w", err)
	}
	defer rows.Close()

	var out []ItemEvent
	for rows.Next() {
		var e ItemEvent
		if err := rows.Scan(&e.ID, &e.ItemID, &e.Kind, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// recordEvent appends one journal entry. Always called inside the transaction
// that performed the change it describes.
func recordEvent(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, kind string,
	fromContainer *uuid.UUID, fromSlot *int, toContainer *uuid.UUID, toSlot *int,
	actor uuid.UUID, tick uint64) error {

	var actorPtr *uuid.UUID
	if actor != uuid.Nil {
		actorPtr = &actor
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO item_events
		    (item_id, kind, from_container, from_slot, to_container, to_slot, actor_character_id, tick)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		itemID, kind, fromContainer, fromSlot, toContainer, toSlot, actorPtr, int64(tick))
	return err
}
