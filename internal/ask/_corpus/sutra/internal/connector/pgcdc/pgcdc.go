// Package pgcdc streams changes from a source Postgres table into the
// warehouse using logical replication (pgoutput).
//
// First run:
//  1. Create a publication for the table and a replication slot with an
//     exported snapshot.
//  2. Copy the table inside that exact snapshot. Every change after it
//     arrives through the slot, so nothing is missed or double-counted.
//
// Streaming: changes are buffered per transaction and applied to the
// warehouse at COMMIT, together with the commit LSN as the checkpoint, in one
// warehouse transaction. Only then is the LSN acknowledged to the source, so
// the source keeps WAL until Sutra has durably applied it. After a crash the
// slot replays from the last acknowledged LSN and anything at or below the
// stored checkpoint is skipped.
package pgcdc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/warehouse"
)

type Connector struct {
	SourceURL string // regular connection string to the source database
	Source    ontology.Source
	WH        *warehouse.DB
	Slot      string
	Pub       string
	Log       *slog.Logger
	// IdleExit stops Run after this long without changes (0 = run forever).
	// Used by `sutra sync --once` and tests.
	IdleExit time.Duration
}

func (c *Connector) names() (slot, pub string) {
	base := "sutra_" + strings.ReplaceAll(c.Source.Name, "-", "_")
	slot, pub = c.Slot, c.Pub
	if slot == "" {
		slot = base
	}
	if pub == "" {
		pub = base + "_pub"
	}
	return
}

func replURL(u string) string {
	if strings.Contains(u, "?") {
		return u + "&replication=database"
	}
	return u + "?replication=database"
}

// Run snapshots (first time) and then streams until ctx is cancelled.
func (c *Connector) Run(ctx context.Context) error {
	slot, pub := c.names()
	cp, err := c.WH.Checkpoint(ctx, c.Source.Name)
	if err != nil {
		return err
	}
	conn, err := pgconn.Connect(ctx, replURL(c.SourceURL))
	if err != nil {
		return fmt.Errorf("replication connect: %w", err)
	}
	defer conn.Close(context.Background())

	var start pglogrepl.LSN
	if cp == "" {
		if start, err = c.snapshot(ctx, conn, slot, pub); err != nil {
			return err
		}
	} else if start, err = pglogrepl.ParseLSN(cp); err != nil {
		return fmt.Errorf("bad checkpoint %q: %w", cp, err)
	}
	return c.stream(ctx, conn, slot, pub, start)
}

func quoteIdent(t string) string {
	parts := strings.Split(t, ".")
	for i, p := range parts {
		parts[i] = pgx.Identifier{p}.Sanitize()
	}
	return strings.Join(parts, ".")
}

func (c *Connector) snapshot(ctx context.Context, repl *pgconn.PgConn, slot, pub string) (pglogrepl.LSN, error) {
	reg, err := pgx.Connect(ctx, c.SourceURL)
	if err != nil {
		return 0, err
	}
	defer reg.Close(context.Background())
	table := quoteIdent(c.Source.Table)
	if _, err := reg.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = %s) THEN
			CREATE PUBLICATION %s FOR TABLE %s;
		END IF; END $$`, quoteLiteral(pub), pgx.Identifier{pub}.Sanitize(), table)); err != nil {
		return 0, fmt.Errorf("create publication: %w", err)
	}
	// A slot left behind by a crashed first run has no usable snapshot.
	_ = pglogrepl.DropReplicationSlot(ctx, repl, slot, pglogrepl.DropReplicationSlotOptions{Wait: true})
	res, err := pglogrepl.CreateReplicationSlot(ctx, repl, slot, "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{SnapshotAction: "EXPORT_SNAPSHOT", Mode: pglogrepl.LogicalReplication})
	if err != nil {
		return 0, fmt.Errorf("create slot: %w", err)
	}
	start, err := pglogrepl.ParseLSN(res.ConsistentPoint)
	if err != nil {
		return 0, err
	}
	tx, err := reg.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET TRANSACTION SNAPSHOT `+quoteLiteral(res.SnapshotName)); err != nil {
		return 0, fmt.Errorf("set snapshot: %w", err)
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT to_jsonb(t) FROM %s t`, table))
	if err != nil {
		return 0, err
	}
	var batch []warehouse.Versioned
	n := 0
	flush := func(cp string) error {
		if _, err := c.WH.Apply(ctx, c.Source.Name, cp, batch); err != nil {
			return err
		}
		n += len(batch)
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var row map[string]any
		if err := rows.Scan(&row); err != nil {
			rows.Close()
			return 0, err
		}
		r, err := c.Source.MapRow(stringify(row))
		if err != nil {
			rows.Close()
			return 0, err
		}
		r.Position = start.String()
		batch = append(batch, warehouse.Versioned{Record: r, Seq: uint64(start)})
		if len(batch) == 500 {
			if err := flush(""); err != nil {
				rows.Close()
				return 0, err
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// The checkpoint is written only with the last batch: a crash during the
	// snapshot restarts it from scratch with a new slot.
	if err := flush(start.String()); err != nil {
		return 0, err
	}
	c.logger().Info("snapshot done", "source", c.Source.Name, "rows", n, "lsn", start)
	return start, nil
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func stringify(row map[string]any) map[string]string {
	out := make(map[string]string, len(row))
	for k, v := range row {
		if v == nil {
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}

func (c *Connector) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Connector) stream(ctx context.Context, conn *pgconn.PgConn, slot, pub string, start pglogrepl.LSN) error {
	err := pglogrepl.StartReplication(ctx, conn, slot, start, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '1'", "publication_names " + quoteLiteral(pub)}})
	if err != nil {
		return fmt.Errorf("start replication: %w", err)
	}
	relations := map[uint32]*pglogrepl.RelationMessage{}
	var pending []warehouse.Versioned
	acked := start
	lastActivity := time.Now()
	ack := func() error {
		return pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{
			WALWritePosition: acked, WALFlushPosition: acked, WALApplyPosition: acked})
	}
	standby := time.NewTicker(10 * time.Second)
	defer standby.Stop()

	for {
		select {
		case <-standby.C:
			if err := ack(); err != nil {
				return err
			}
		default:
		}
		if c.IdleExit > 0 && len(pending) == 0 && time.Since(lastActivity) > c.IdleExit {
			return ack()
		}
		rctx, cancel := context.WithTimeout(ctx, time.Second)
		msg, err := conn.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) && ctx.Err() == nil {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("replication error: %s", m.Message)
		case *pgproto3.CopyData:
			switch m.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				ka, err := pglogrepl.ParsePrimaryKeepaliveMessage(m.Data[1:])
				if err != nil {
					return err
				}
				if ka.ReplyRequested {
					if err := ack(); err != nil {
						return err
					}
				}
			case pglogrepl.XLogDataByteID:
				xld, err := pglogrepl.ParseXLogData(m.Data[1:])
				if err != nil {
					return err
				}
				lm, err := pglogrepl.Parse(xld.WALData)
				if err != nil {
					return err
				}
				lastActivity = time.Now()
				switch v := lm.(type) {
				case *pglogrepl.RelationMessage:
					relations[v.RelationID] = v
				case *pglogrepl.BeginMessage:
					pending = pending[:0]
				case *pglogrepl.InsertMessage:
					if r, ok, err := c.record(relations, v.RelationID, v.Tuple, false, xld.WALStart); err != nil {
						return err
					} else if ok {
						pending = append(pending, r)
					}
				case *pglogrepl.UpdateMessage:
					if r, ok, err := c.record(relations, v.RelationID, v.NewTuple, false, xld.WALStart); err != nil {
						return err
					} else if ok {
						pending = append(pending, r)
					}
				case *pglogrepl.DeleteMessage:
					if r, ok, err := c.record(relations, v.RelationID, v.OldTuple, true, xld.WALStart); err != nil {
						return err
					} else if ok {
						pending = append(pending, r)
					}
				case *pglogrepl.CommitMessage:
					end := v.TransactionEndLSN
					// After a restart the slot can resend transactions that were
					// applied but not yet acknowledged; the checkpoint says so.
					if end > start {
						if _, err := c.WH.Apply(ctx, c.Source.Name, end.String(), pending); err != nil {
							return fmt.Errorf("apply: %w", err)
						}
						if len(pending) > 0 {
							c.logger().Info("applied", "source", c.Source.Name, "changes", len(pending), "lsn", end)
						}
					}
					pending = pending[:0]
					acked = end
					if err := ack(); err != nil {
						return err
					}
				}
			}
		}
	}
}

var errNoRelation = errors.New("change for unknown relation")

func (c *Connector) record(rels map[uint32]*pglogrepl.RelationMessage, id uint32, t *pglogrepl.TupleData, deleted bool, lsn pglogrepl.LSN) (warehouse.Versioned, bool, error) {
	rel, ok := rels[id]
	if !ok {
		return warehouse.Versioned{}, false, errNoRelation
	}
	if t == nil {
		return warehouse.Versioned{}, false, nil
	}
	row := map[string]string{}
	for i, col := range t.Columns {
		if i >= len(rel.Columns) {
			break
		}
		if col.DataType == pglogrepl.TupleDataTypeText {
			row[rel.Columns[i].Name] = string(col.Data)
		}
	}
	r, err := c.Source.MapRow(row)
	if err != nil {
		return warehouse.Versioned{}, false, err
	}
	r.Deleted = deleted
	r.Position = lsn.String()
	return warehouse.Versioned{Record: r, Seq: uint64(lsn)}, true, nil
}
