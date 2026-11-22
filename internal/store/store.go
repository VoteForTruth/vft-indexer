// Package store keeps indexed data in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

// Open opens the SQLite database file at path, creating it if needed.
//
// One process writes: the scanner applies transactions one by one; moderation
// commands from another process wait on busy_timeout. WAL lets the API read
// while the scanner writes. Write transactions take the lock up front
// (_txlock=immediate), so a reader never has to be upgraded to a writer midway.
// Times are written as "2006-01-02 15:04:05.999999999-07:00" (_time_format);
// the store always passes UTC, so they also compare correctly as text.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_time_format=sqlite&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}

	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() {
	s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

// inTx runs fn in a transaction, committing on success.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Migrate applies migrations/*.sql in name order, each once.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists schema_migrations (name text primary key)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		var applied bool
		err = s.db.QueryRowContext(ctx, `select exists(select 1 from schema_migrations where name = ?1)`, e.Name()).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		script, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}

		err = s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `insert into schema_migrations (name) values (?1)`, e.Name())
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Contracts
// ---------------------------------------------------------------------------

type ContractKind string

const (
	KindMaster    ContractKind = "master"
	KindStatement ContractKind = "statement"
	KindVote      ContractKind = "vote"
)

// Contract is a known VFT contract. Fields not relevant to the kind are zero.
type Contract struct {
	Address string
	Kind    ContractKind

	// statement
	Parent string
	Idx    uint32
	Pro    bool

	// vote
	Statement string
	Owner     string

	NextIdx uint32
	NextPro uint32
	NextCon uint32
}

// LoadContracts returns all known contracts by address.
func (s *Store) LoadContracts(ctx context.Context) (map[string]*Contract, error) {
	rows, err := s.db.QueryContext(ctx, `
		select address, kind, coalesce(parent, ''), coalesce(idx, 0), coalesce(pro, false),
		       coalesce(statement, ''), coalesce(owner, ''), next_idx, next_pro, next_con
		from contracts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	contracts := map[string]*Contract{}
	for rows.Next() {
		var c Contract
		err = rows.Scan(&c.Address, &c.Kind, &c.Parent, &c.Idx, &c.Pro, &c.Statement, &c.Owner, &c.NextIdx, &c.NextPro, &c.NextCon)
		if err != nil {
			return nil, err
		}
		contracts[c.Address] = &c
	}
	return contracts, rows.Err()
}

// Tx is a database transaction for applying one blockchain transaction.
type Tx struct {
	tx *sql.Tx
}

// Apply runs fn in a database transaction.
func (s *Store) Apply(ctx context.Context, fn func(tx Tx) error) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return fn(Tx{tx: tx})
	})
}

// MarkProcessed records a blockchain transaction as applied.
// It returns false if it was applied before.
func (t Tx) MarkProcessed(ctx context.Context, account string, lt uint64) (bool, error) {
	res, err := t.tx.ExecContext(ctx, `insert into processed_txs (account, lt) values (?1, ?2) on conflict do nothing`, account, int64(lt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (t Tx) InsertContract(ctx context.Context, c *Contract) error {
	_, err := t.tx.ExecContext(ctx, `
		insert into contracts (address, kind, parent, idx, pro, statement, owner)
		values (?1, ?2, nullif(?3, ''), ?4, ?5, nullif(?6, ''), nullif(?7, ''))
		on conflict (address) do nothing`,
		c.Address, c.Kind, c.Parent, nullableIdx(c), nullablePro(c), c.Statement, c.Owner)
	return err
}

func nullableIdx(c *Contract) any {
	if c.Kind != KindStatement {
		return nil
	}
	return c.Idx
}

func nullablePro(c *Contract) any {
	if c.Kind != KindStatement {
		return nil
	}
	return c.Pro
}

func (t Tx) UpdateCounters(ctx context.Context, c *Contract) error {
	_, err := t.tx.ExecContext(ctx, `update contracts set next_idx = ?2, next_pro = ?3, next_con = ?4 where address = ?1`,
		c.Address, c.NextIdx, c.NextPro, c.NextCon)
	return err
}

// ---------------------------------------------------------------------------
// Statements and events
// ---------------------------------------------------------------------------

type Statement struct {
	Address   string
	Root      string
	Parent    string
	Depth     int
	Idx       uint32
	Pro       bool
	Author    string
	Text      string
	URL       *string
	CreatedAt time.Time
	CreatedLT uint64
	Hidden    bool
}

func (t Tx) InsertStatement(ctx context.Context, st Statement) error {
	_, err := t.tx.ExecContext(ctx, `
		insert into statements (address, root, parent, depth, idx, pro, author, text, url, created_at, created_lt)
		values (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)
		on conflict (address) do nothing`,
		st.Address, st.Root, st.Parent, st.Depth, st.Idx, st.Pro, st.Author, st.Text, st.URL, st.CreatedAt.UTC(), int64(st.CreatedLT))
	return err
}

// StatementPosition returns root and depth of a statement, to place its children.
func (t Tx) StatementPosition(ctx context.Context, address string) (root string, depth int, err error) {
	err = t.tx.QueryRowContext(ctx, `select root, depth from statements where address = ?1`, address).Scan(&root, &depth)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	return root, depth, err
}

type EventKind string

const (
	EventVoteAdded      EventKind = "vote_added"
	EventVoteWithdrawn  EventKind = "vote_withdrawn"
	EventVoteRevokedAll EventKind = "vote_revoked_all"
	EventDonation       EventKind = "donation"
)

type Event struct {
	Account   string
	LT        uint64
	TxHash    []byte
	Time      time.Time
	Kind      EventKind
	Statement string
	Wallet    string
	Amount    *big.Int // nil for vote_revoked_all
}

func (t Tx) InsertEvent(ctx context.Context, e Event) error {
	var amount any
	if e.Amount != nil {
		amount = e.Amount.String()
	}
	_, err := t.tx.ExecContext(ctx, `
		insert into events (account, lt, tx_hash, time, kind, statement, wallet, amount)
		values (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)`,
		e.Account, int64(e.LT), e.TxHash, e.Time.UTC(), e.Kind, e.Statement, e.Wallet, amount)
	return err
}

// ---------------------------------------------------------------------------
// Cursor
// ---------------------------------------------------------------------------

// Cursor returns the last fully processed masterchain seqno.
func (s *Store) Cursor(ctx context.Context) (uint32, error) {
	var seqno int64
	err := s.db.QueryRowContext(ctx, `select mc_seqno from scan_cursor where id = 1`).Scan(&seqno)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return uint32(seqno), err
}

func (s *Store) SetCursor(ctx context.Context, seqno uint32) error {
	_, err := s.db.ExecContext(ctx, `
		insert into scan_cursor (id, mc_seqno) values (1, ?1)
		on conflict (id) do update set mc_seqno = excluded.mc_seqno`, int64(seqno))
	return err
}

// ---------------------------------------------------------------------------
// Moderation
// ---------------------------------------------------------------------------

// SetHidden hides a statement from the API (or shows it again). The contract
// itself is not affected. Returns ErrNotFound for an unknown statement.
func (s *Store) SetHidden(ctx context.Context, address string, hidden bool, reason string) error {
	var res sql.Result
	var err error
	if hidden {
		res, err = s.db.ExecContext(ctx, `update statements set hidden = true, hidden_reason = ?2, hidden_at = ?3 where address = ?1`,
			address, reason, time.Now().UTC())
	} else {
		res, err = s.db.ExecContext(ctx, `update statements set hidden = false, hidden_reason = null, hidden_at = null where address = ?1`, address)
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
