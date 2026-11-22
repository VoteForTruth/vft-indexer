// Package api serves the indexed data as a read-only JSON API for the Mini App.
//
// Amounts and weights are strings with nanotons. Contract addresses are
// bounceable, wallet addresses are non-bounceable (user-friendly form).
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/xssnick/tonutils-go/address"

	"github.com/VoteForTruth/vft-indexer/internal/store"
)

type Server struct {
	read    reader
	testnet bool
	log     *slog.Logger
	now     func() time.Time // weights depend on time; tests pin it
}

func New(db *sql.DB, testnet bool, log *slog.Logger, now func() time.Time) *Server {
	return &Server{read: reader{db: db}, testnet: testnet, log: log, now: now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /statements", s.handle(s.feed))
	mux.HandleFunc("GET /statements/{address}", s.handle(s.statementDetail))
	mux.HandleFunc("GET /statements/{address}/votes", s.handle(s.statementVotes))
	mux.HandleFunc("GET /wallets/{address}/votes", s.handle(s.walletVotes))
	mux.HandleFunc("GET /wallets/{address}/donations", s.handle(s.walletDonations))
	mux.HandleFunc("GET /wallets/{address}/statements", s.handle(s.walletStatements))
	mux.HandleFunc("GET /moderation", s.handle(s.moderation))
	mux.HandleFunc("GET /health", s.handle(s.health))
	return cors(mux)
}

// the API is public and read-only, so any origin may call it
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		next.ServeHTTP(w, r)
	})
}

type httpError struct {
	status int
	body   any
}

func (e httpError) Error() string { return http.StatusText(e.status) }

func badRequest(msg string) error {
	return httpError{http.StatusBadRequest, map[string]string{"error": msg}}
}

var notFound = httpError{http.StatusNotFound, map[string]string{"error": "not found"}}

func (s *Server) handle(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := fn(r)
		status := http.StatusOK

		var he httpError
		switch {
		case errors.As(err, &he):
			status, body = he.status, he.body
		case err != nil:
			s.log.Error("api", "path", r.URL.Path, "err", err)
			status, body = http.StatusInternalServerError, map[string]string{"error": "internal error"}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

type votesJSON struct {
	Amount    string  `json:"amount"`
	Weight    string  `json:"weight"`
	Voters    int     `json:"voters"`
	Top1Share float64 `json:"top1_share"`
	Top3Share float64 `json:"top3_share"`
}

type argumentsJSON struct {
	Pro int `json:"pro"`
	Con int `json:"con"`
}

// resultJSON is the outcome of a dispute: votes for its direct pro vs con arguments.
type resultJSON struct {
	ProWeight string `json:"pro_weight"`
	ConWeight string `json:"con_weight"`
	ProAmount string `json:"pro_amount"`
	ConAmount string `json:"con_amount"`
}

type statementJSON struct {
	Address   string        `json:"address"`
	Root      string        `json:"root"`
	Parent    string        `json:"parent"`
	Depth     int           `json:"depth"`
	Index     int           `json:"index"`
	Side      string        `json:"side"` // "pro" or "con"; roots are "pro"
	Author    string        `json:"author"`
	Text      string        `json:"text"`
	URL       *string       `json:"url"`
	CreatedAt time.Time     `json:"created_at"`
	Votes     votesJSON     `json:"votes"`
	Donations string        `json:"donations"`
	Arguments argumentsJSON `json:"arguments"`
	Result    resultJSON    `json:"result"`
	Activity  *int          `json:"activity_24h,omitempty"`
	Hidden    *hiddenJSON   `json:"hidden,omitempty"` // only in the author's own list
}

func (s *Server) contract(raw string) string {
	a, err := address.ParseRawAddr(raw)
	if err != nil {
		return raw
	}
	return a.Testnet(s.testnet).Bounce(true).String()
}

func (s *Server) wallet(raw string) string {
	a, err := address.ParseRawAddr(raw)
	if err != nil {
		return raw
	}
	return a.Testnet(s.testnet).Bounce(false).String()
}

// parseAddress accepts user-friendly and raw forms and returns the raw form.
func parseAddress(r *http.Request) (string, error) {
	text := r.PathValue("address")
	a, err := address.ParseAddr(text)
	if err != nil {
		if a, err = address.ParseRawAddr(text); err != nil {
			return "", badRequest("invalid address")
		}
	}
	return a.StringRaw(), nil
}

func side(pro bool) string {
	if pro {
		return "pro"
	}
	return "con"
}

// tree is everything needed to render statements: the statements themselves,
// their direct children and tallies of both.
type tree struct {
	children map[string][]statementRow
	tallies  map[string]*tally
	now      time.Time
}

func (s *Server) loadTree(r *http.Request, statements []statementRow) (tree, error) {
	ctx := r.Context()
	addrs := make([]string, 0, len(statements))
	for _, st := range statements {
		addrs = append(addrs, st.Address)
	}

	children, err := s.read.children(ctx, addrs)
	if err != nil {
		return tree{}, err
	}

	t := tree{children: map[string][]statementRow{}, now: s.now()}
	all := append([]string{}, addrs...)
	for _, c := range children {
		t.children[c.Parent] = append(t.children[c.Parent], c)
		all = append(all, c.Address)
	}

	if t.tallies, err = s.read.tallies(ctx, all); err != nil {
		return tree{}, err
	}
	return t, nil
}

func (s *Server) statementJSON(st statementRow, t tree) statementJSON {
	stats := t.tallies[st.Address].stats(t.now)

	var args argumentsJSON
	proW, conW, proA, conA := new(big.Int), new(big.Int), new(big.Int), new(big.Int)
	for _, c := range t.children[st.Address] {
		cs := t.tallies[c.Address].stats(t.now)
		if c.Pro {
			args.Pro++
			proW.Add(proW, cs.weight)
			proA.Add(proA, cs.amount)
		} else {
			args.Con++
			conW.Add(conW, cs.weight)
			conA.Add(conA, cs.amount)
		}
	}

	return statementJSON{
		Address:   s.contract(st.Address),
		Root:      s.contract(st.Root),
		Parent:    s.contract(st.Parent),
		Depth:     st.Depth,
		Index:     st.Idx,
		Side:      side(st.Pro),
		Author:    s.wallet(st.Author),
		Text:      st.Text,
		URL:       st.URL,
		CreatedAt: st.CreatedAt,
		Votes: votesJSON{
			Amount:    stats.amount.String(),
			Weight:    stats.weight.String(),
			Voters:    stats.voters,
			Top1Share: stats.top1Share,
			Top3Share: stats.top3Share,
		},
		Donations: t.tallies[st.Address].donations.String(),
		Arguments: args,
		Result:    resultJSON{ProWeight: proW.String(), ConWeight: conW.String(), ProAmount: proA.String(), ConAmount: conA.String()},
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GET /statements?sort=weight|new|active&limit=50&offset=0
func (s *Server) feed(r *http.Request) (any, error) {
	limit, offset, err := page(r)
	if err != nil {
		return nil, err
	}
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "weight"
	}
	if sortBy != "weight" && sortBy != "new" && sortBy != "active" {
		return nil, badRequest("sort must be weight, new or active")
	}

	roots, err := s.read.roots(r.Context())
	if err != nil {
		return nil, err
	}
	t, err := s.loadTree(r, roots)
	if err != nil {
		return nil, err
	}
	activity, err := s.read.activity(r.Context(), t.now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}

	list := make([]statementJSON, 0, len(roots))
	for _, root := range roots {
		js := s.statementJSON(root, t)
		n := activity[root.Address]
		js.Activity = &n
		list = append(list, js)
	}

	sort.SliceStable(list, func(i, j int) bool {
		switch sortBy {
		case "new":
			return list[i].CreatedAt.After(list[j].CreatedAt)
		case "active":
			if *list[i].Activity != *list[j].Activity {
				return *list[i].Activity > *list[j].Activity
			}
		}
		if c := cmpDecimal(list[i].Votes.Weight, list[j].Votes.Weight); c != 0 {
			return c > 0
		}
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})

	if offset > len(list) {
		offset = len(list)
	}
	end := min(offset+limit, len(list))
	return map[string]any{"total": len(list), "statements": list[offset:end]}, nil
}

// GET /statements/{address}: the statement with its arguments, each side sorted by weight.
func (s *Server) statementDetail(r *http.Request) (any, error) {
	addr, err := parseAddress(r)
	if err != nil {
		return nil, err
	}
	st, err := s.read.statement(r.Context(), addr)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if st.Hidden {
		return nil, httpError{http.StatusGone, map[string]any{"hidden": true, "reason": st.Reason}}
	}

	t, err := s.loadTree(r, []statementRow{st})
	if err != nil {
		return nil, err
	}
	// arguments need their own children for argument counts and results
	children := t.children[st.Address]
	childTree, err := s.loadTree(r, children)
	if err != nil {
		return nil, err
	}

	pro, con := []statementJSON{}, []statementJSON{}
	for _, c := range children {
		js := s.statementJSON(c, childTree)
		if c.Pro {
			pro = append(pro, js)
		} else {
			con = append(con, js)
		}
	}
	byWeight := func(list []statementJSON) {
		sort.SliceStable(list, func(i, j int) bool {
			if c := cmpDecimal(list[i].Votes.Weight, list[j].Votes.Weight); c != 0 {
				return c > 0
			}
			return list[i].Index < list[j].Index
		})
	}
	byWeight(pro)
	byWeight(con)

	return map[string]any{"statement": s.statementJSON(st, t), "pro": pro, "con": con}, nil
}

type voterJSON struct {
	Wallet string    `json:"wallet"`
	Amount string    `json:"amount"`
	Weight string    `json:"weight"`
	Since  time.Time `json:"since"`
}

// GET /statements/{address}/votes: who supports the statement, largest first.
func (s *Server) statementVotes(r *http.Request) (any, error) {
	addr, err := parseAddress(r)
	if err != nil {
		return nil, err
	}
	if _, err = s.read.statement(r.Context(), addr); errors.Is(err, store.ErrNotFound) {
		return nil, notFound
	} else if err != nil {
		return nil, err
	}

	tallies, err := s.read.tallies(r.Context(), []string{addr})
	if err != nil {
		return nil, err
	}
	list := []voterJSON{}
	for _, v := range tallies[addr].voters(s.now()) {
		list = append(list, voterJSON{Wallet: s.wallet(v.wallet), Amount: v.amount.String(), Weight: v.weight.String(), Since: v.since})
	}
	return map[string]any{"votes": list}, nil
}

type walletVoteJSON struct {
	Statement string    `json:"statement"`
	Root      string    `json:"root"`
	Text      string    `json:"text"`
	Side      string    `json:"side"`
	Depth     int       `json:"depth"`
	Amount    string    `json:"amount"`
	Weight    string    `json:"weight"`
	Since     time.Time `json:"since"`
}

// GET /wallets/{address}/votes: current votes of a wallet.
func (s *Server) walletVotes(r *http.Request) (any, error) {
	wallet, err := parseAddress(r)
	if err != nil {
		return nil, err
	}
	statements, err := s.read.walletStatements(r.Context(), wallet, []string{
		string(store.EventVoteAdded), string(store.EventVoteWithdrawn), string(store.EventVoteRevokedAll),
	})
	if err != nil {
		return nil, err
	}
	tallies, err := s.read.tallies(r.Context(), statements)
	if err != nil {
		return nil, err
	}

	now := s.now()
	list := []walletVoteJSON{}
	for _, addr := range statements {
		v := tallies[addr].votes[wallet]
		if v == nil || v.Amount().Sign() == 0 {
			continue
		}
		st, err := s.read.statement(r.Context(), addr)
		if err != nil {
			return nil, err
		}
		// the wallet's own votes are shown even on hidden statements, so the money can be found and revoked
		list = append(list, walletVoteJSON{
			Statement: s.contract(addr),
			Root:      s.contract(st.Root),
			Text:      st.Text,
			Side:      side(st.Pro),
			Depth:     st.Depth,
			Amount:    v.Amount().String(),
			Weight:    v.Weight(now).String(),
			Since:     v.Since(),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Since.After(list[j].Since) })
	return map[string]any{"votes": list}, nil
}

type hiddenJSON struct {
	Reason *string `json:"reason"`
}

// GET /wallets/{address}/statements: statements and arguments written by the wallet,
// newest first. Hidden ones are included and marked, so the author knows why.
func (s *Server) walletStatements(r *http.Request) (any, error) {
	author, err := parseAddress(r)
	if err != nil {
		return nil, err
	}
	statements, err := s.read.byAuthor(r.Context(), author)
	if err != nil {
		return nil, err
	}
	t, err := s.loadTree(r, statements)
	if err != nil {
		return nil, err
	}

	list := []statementJSON{}
	for _, st := range statements {
		js := s.statementJSON(st, t)
		if st.Hidden {
			js.Hidden = &hiddenJSON{Reason: st.Reason}
		}
		list = append(list, js)
	}
	return map[string]any{"statements": list}, nil
}

type donationJSON struct {
	Statement string    `json:"statement"`
	Donor     string    `json:"donor"`
	Amount    string    `json:"amount"`
	Time      time.Time `json:"time"`
}

// GET /wallets/{address}/donations: donations received as an author and sent as a donor.
func (s *Server) walletDonations(r *http.Request) (any, error) {
	wallet, err := parseAddress(r)
	if err != nil {
		return nil, err
	}
	received, err := s.read.donations(r.Context(), "s.author = ?1", wallet)
	if err != nil {
		return nil, err
	}
	sent, err := s.read.donations(r.Context(), "e.wallet = ?1", wallet)
	if err != nil {
		return nil, err
	}

	toJSON := func(rows []donationRow) []donationJSON {
		list := []donationJSON{}
		for _, d := range rows {
			list = append(list, donationJSON{Statement: s.contract(d.Statement), Donor: s.wallet(d.Donor), Amount: d.Amount, Time: d.Time})
		}
		return list
	}
	return map[string]any{"received": toJSON(received), "sent": toJSON(sent)}, nil
}

// GET /moderation: the public log of hidden statements. The text is not repeated here.
func (s *Server) moderation(r *http.Request) (any, error) {
	hidden, err := s.read.hidden(r.Context())
	if err != nil {
		return nil, err
	}
	list := []map[string]any{}
	for _, h := range hidden {
		list = append(list, map[string]any{"statement": s.contract(h.Address), "reason": h.Reason, "hidden_at": h.HiddenAt})
	}
	return map[string]any{"hidden": list}, nil
}

// GET /health: the last indexed masterchain block.
func (s *Server) health(r *http.Request) (any, error) {
	var seqno int64
	err := s.read.db.QueryRowContext(r.Context(), `select coalesce((select mc_seqno from scan_cursor where id = 1), 0)`).Scan(&seqno)
	if err != nil {
		return nil, err
	}
	return map[string]any{"mc_seqno": seqno}, nil
}

func page(r *http.Request) (limit, offset int, err error) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 200 {
			return 0, 0, badRequest("limit must be 1..200")
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil || offset < 0 {
			return 0, 0, badRequest("offset must be >= 0")
		}
	}
	return limit, offset, nil
}

func cmpDecimal(a, b string) int {
	x, _ := new(big.Int).SetString(a, 10)
	y, _ := new(big.Int).SetString(b, 10)
	return x.Cmp(y)
}
