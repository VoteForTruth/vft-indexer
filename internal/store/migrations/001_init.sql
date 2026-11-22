-- Addresses are stored in raw form "0:<hex>", amounts in nanotons as decimal
-- text (exact, no overflow), times as UTC text ("2006-01-02 15:04:05.999999999+00:00"):
-- with one offset everywhere they compare correctly as strings.

-- Every VFT contract the indexer knows about, discovered from deploys.
create table contracts (
    address   text primary key,
    kind      text not null check (kind in ('master', 'statement', 'vote')),
    -- statement: its parent (master or statement), index and side
    parent    text,
    idx       integer,
    pro       boolean,
    -- vote: (statement, owner)
    statement text,
    owner     text,
    -- counters mirrored from the contract, used to derive the next child address
    next_idx  integer not null default 0, -- master: roots
    next_pro  integer not null default 0, -- statement: pro arguments
    next_con  integer not null default 0  -- statement: con arguments
);

create table statements (
    address       text primary key references contracts (address),
    root          text not null,  -- the root statement (topic) of the tree
    parent        text not null,  -- master for roots
    depth         integer not null, -- 0 for roots
    idx           integer not null,
    pro           boolean not null,
    author        text not null,
    text          text not null,
    url           text,
    created_at    timestamp not null,
    created_lt    integer not null,
    hidden        boolean not null default false,
    hidden_reason text
);

create index statements_parent on statements (parent);
create index statements_root on statements (root);

-- The source of truth: everything else is derived from events.
create table events (
    id        integer primary key,
    account   text not null,   -- contract whose transaction produced the event
    lt        integer not null,
    tx_hash   blob not null,
    time      timestamp not null,
    kind      text not null check (kind in ('vote_added', 'vote_withdrawn', 'vote_revoked_all', 'donation')),
    statement text not null,
    wallet    text not null,   -- voter or donor
    amount    text,            -- decimal nanotons; null for vote_revoked_all
    unique (account, lt)
);

create index events_statement on events (statement, lt);
create index events_wallet on events (wallet, lt);

-- Transactions already applied, so a replayed block changes nothing.
create table processed_txs (
    account text not null,
    lt      integer not null,
    primary key (account, lt)
);

create table scan_cursor (
    id       integer primary key check (id = 1),
    mc_seqno integer not null
);
