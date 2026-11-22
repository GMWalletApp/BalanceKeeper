PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS watches (
 id INTEGER PRIMARY KEY AUTOINCREMENT, network TEXT NOT NULL, address TEXT NOT NULL, initial TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', start INTEGER NOT NULL DEFAULT -1, upstream_start INTEGER NOT NULL DEFAULT -1, discovery_due INTEGER NOT NULL DEFAULT 0, UNIQUE(network,address)
);
CREATE TABLE IF NOT EXISTS assets (
 watch INTEGER NOT NULL REFERENCES watches ON DELETE CASCADE, token TEXT NOT NULL, name TEXT NOT NULL,
 amount TEXT, height INTEGER, last_error TEXT NOT NULL DEFAULT '', derived TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(watch,token)
);
CREATE TABLE IF NOT EXISTS snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, watch INTEGER NOT NULL REFERENCES watches ON DELETE CASCADE,
 token TEXT NOT NULL,name TEXT NOT NULL, amount TEXT NOT NULL, decimals INTEGER,height INTEGER,
 block_time TEXT, observed INTEGER NOT NULL,source TEXT NOT NULL,event TEXT, observation TEXT,
 UNIQUE(watch,token,observation)
);
CREATE INDEX IF NOT EXISTS history ON snapshots(watch,id DESC);
CREATE INDEX IF NOT EXISTS token_history ON snapshots(watch,token,id DESC);
CREATE TABLE IF NOT EXISTS jobs(
 id INTEGER PRIMARY KEY,watch INTEGER NOT NULL REFERENCES watches ON DELETE CASCADE,token TEXT NOT NULL,
 source TEXT NOT NULL, observation TEXT NOT NULL, min_height INTEGER NOT NULL, event TEXT,
 due INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0, UNIQUE(watch,token,observation)
);
CREATE TABLE IF NOT EXISTS sources(
 watch INTEGER NOT NULL REFERENCES watches ON DELETE CASCADE,token TEXT NOT NULL,observation TEXT NOT NULL,
 source TEXT NOT NULL,event TEXT NOT NULL, PRIMARY KEY(watch,token,observation,source,event)
);
CREATE TABLE IF NOT EXISTS inbox (
 id INTEGER PRIMARY KEY, source TEXT NOT NULL, identity TEXT NOT NULL, digest TEXT NOT NULL,
 network TEXT NOT NULL, payload TEXT NOT NULL, ceiling INTEGER NOT NULL, done INTEGER NOT NULL DEFAULT 0,
 due INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0, UNIQUE(source,identity)
);
CREATE INDEX IF NOT EXISTS pending_inbox ON inbox(due,id) WHERE done=0;
CREATE INDEX IF NOT EXISTS pending_jobs ON jobs(due,id);
CREATE INDEX IF NOT EXISTS derived_wallet ON assets(derived) WHERE derived<>'';

-- Temporary removal barrier: an immediate re-registration gets a fresh upstream interval.
CREATE TABLE IF NOT EXISTS removals (generation INTEGER PRIMARY KEY AUTOINCREMENT, network TEXT NOT NULL, address TEXT NOT NULL, UNIQUE(network,address));
CREATE TABLE IF NOT EXISTS gaps (
 watch INTEGER PRIMARY KEY REFERENCES watches ON DELETE CASCADE, cursor INTEGER NOT NULL,
 end INTEGER NOT NULL, due INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT ''
);

-- Unclassified contracts touched by a transaction are isolated from real holdings.
CREATE TABLE IF NOT EXISTS evm_probes (
 id INTEGER PRIMARY KEY, watch INTEGER NOT NULL REFERENCES watches ON DELETE CASCADE,
 token TEXT NOT NULL, observation TEXT NOT NULL,height INTEGER NOT NULL, source TEXT NOT NULL,
 event TEXT NOT NULL, due INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
 UNIQUE(watch,token,observation)
);
CREATE INDEX IF NOT EXISTS due_probes ON evm_probes(due,id);
