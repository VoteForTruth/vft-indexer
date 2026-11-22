# vft-indexer

Indexer of the **Vote For Truth** contracts ([vft-contract](https://github.com/VoteForTruth/vft-contract)): scans TON blocks, stores the history of statements, votes and donations in SQLite, and serves it as a read-only JSON API for the Mini App.

The indexer never sends transactions: the app talks to the contracts directly through TON Connect and only reads from here.

## How it works

- **Blocks, not account histories.** A vote contract is destroyed on a full revoke, and the history of a destroyed account can't be listed anymore. The scanner walks masterchain blocks in order and takes every shard block they commit, so nothing is missed.
- **Contracts are discovered, not configured.** Only the master address is given. Every statement and vote is deployed by an already known contract; the indexer re-derives the child's address from the embedded contract code and checks that the parent really sent a message there.
- **Events are the source of truth.** Votes added/withdrawn/revoked and donations are stored as events. Weights and rankings are computed from events on request, so formulas can change without re-indexing.
- Only successful transactions count. Replaying blocks is harmless: every transaction is applied once.

### Vote weight

Every addition to a vote is a lot that grows linearly to its full amount over **7 days**. Revoking takes the newest lots first, so money added before a news peak and pulled out soon after never weighs much, while a long-standing vote keeps its weight. The API returns both the amount and the weight.

The result of a dispute is the weight of its direct pro arguments vs its direct con arguments. Concentration is the share of the largest 1 and 3 voters in the amount.

### Moderation

Hidden statements are not returned by the API (the contract is not affected). Only illegal content and spam are hidden, never opinions; the public log is at `GET /moderation`.

```bash
vft-indexer -db ... hide <statement> "<reason>"
vft-indexer -db ... unhide <statement>
```

## API

Amounts and weights are strings with nanotons. Contract addresses are bounceable, wallets non-bounceable.

| Endpoint | |
|---|---|
| `GET /statements?sort=weight\|new\|active&limit=50&offset=0` | root statements (topics) |
| `GET /statements/{address}` | a statement with its pro and con arguments, each side sorted by weight |
| `GET /statements/{address}/votes` | voters, largest first |
| `GET /wallets/{address}/votes` | current votes of a wallet |
| `GET /wallets/{address}/statements` | statements and arguments written by a wallet, hidden ones marked |
| `GET /wallets/{address}/donations` | donations received as an author and sent |
| `GET /moderation` | hidden statements with reasons |
| `GET /health` | last indexed masterchain block |

## Running

```bash
make build
./vft-indexer -network testnet -master <VftMaster address>      # API on :8080, data in ./vft-indexer.db
```

On the first run the start block is found from the master deploy time. Settings are flags or env vars: `VFT_NETWORK`, `VFT_MASTER`, `VFT_DB` (SQLite file), `VFT_HTTP`.

**Storage.** One SQLite file (WAL mode, pure-Go driver `modernc.org/sqlite`, no cgo). There is one writer — the scanner applies transactions in order — and the API reads concurrently. The chain is the source of truth, so a lost database is rebuilt by rescanning from the master deploy. Back up with `sqlite3 vft-indexer.db ".backup copy.db"` (a plain copy of a live WAL database may be inconsistent).

Mainnet history older than what public lite servers keep needs an archive lite server.

## Development

```bash
make test          # each test gets its own SQLite file in a temp dir
make contracts     # after changing the contracts: copy build artifacts from ../vft-contract
```

`internal/vft/testdata` holds real testnet transactions of a full scenario; parsing and processing are tested against them.
