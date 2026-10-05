// sqlsoak runs a verifying load against SQL Server for the blkmap Windows soak; it runs
// outside the machine under test, so its record of acknowledged commits survives any crash.
//
//	sqlsoak init   -addr HOST:PORT              create the soak database (FULL recovery)
//	sqlsoak run    -addr HOST:PORT [-for DUR]   commit transactions until killed, printing
//	                                            "ack N" once SQL Server acknowledged seq N
//	sqlsoak verify -addr HOST:PORT -acked N     every seq up to the newest is there with a
//	                                            valid checksum, N is among them, CHECKDB clean
//
// Each transaction inserts row seq with a payload derived from (seq, rev) and its CRC,
// rewrites an earlier row (rev+1, new payload, new CRC) and every 50th also stores a
// 256 KiB blob, so a torn or lost page shows as a missing row or a checksum mismatch.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

const (
	password  = "Blkmap-Lab-2026!" // lab credentials, see scripts/windows/setup.ps1
	database  = "soak"
	blobEvery = 50
	blobSize  = 256 << 10
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sqlsoak init|run|verify -addr HOST:PORT [-acked N] [-for DUR]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	addr := fs.String("addr", "", "SQL Server host:port")
	acked := fs.Int64("acked", -1, "verify: the newest acknowledged seq")
	dur := fs.Duration("for", 0, "run: stop after this long (0: until killed)")
	fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "init":
		err = initDB(*addr)
	case "run":
		err = run(*addr, *dur)
	case "verify":
		err = verify(*addr, *acked)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqlsoak:", err)
		os.Exit(1)
	}
}

func open(addr, db string) (*sql.DB, error) {
	// No connection timeout: CHECKDB in a busy nested guest takes minutes; dials still time out
	return sql.Open("sqlserver", fmt.Sprintf("sqlserver://sa:%s@%s?database=%s&encrypt=disable&connection+timeout=0&dial+timeout=10", password, addr, db))
}

func initDB(addr string) error {
	master, err := open(addr, "master")
	if err != nil {
		return err
	}
	defer master.Close()
	if _, err := master.Exec(`IF DB_ID('soak') IS NULL CREATE DATABASE soak; ALTER DATABASE soak SET RECOVERY FULL; ALTER DATABASE soak SET PAGE_VERIFY CHECKSUM`); err != nil {
		return err
	}
	db, err := open(addr, database)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`
IF OBJECT_ID('ledger') IS NULL CREATE TABLE ledger (seq BIGINT PRIMARY KEY, rev INT NOT NULL, payload VARBINARY(512) NOT NULL, crc BIGINT NOT NULL);
IF OBJECT_ID('blobs') IS NULL CREATE TABLE blobs (seq BIGINT PRIMARY KEY, data VARBINARY(MAX) NOT NULL, crc BIGINT NOT NULL);`)
	if err == nil {
		fmt.Println("initialized")
	}
	return err
}

// payload is what row seq holds at revision rev.
func payload(seq int64, rev int, n int) []byte {
	r := rand.New(rand.NewPCG(uint64(seq), uint64(rev)))
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(r.Uint32())
	}
	return p
}

func checksum(p []byte) int64 {
	return int64(crc32.ChecksumIEEE(p))
}

// run commits until killed or dur passes; connection errors are retried, since the server
// may be restarting with the machine.
func run(addr string, dur time.Duration) error {
	deadline := time.Time{}
	if dur > 0 {
		deadline = time.Now().Add(dur)
	}
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	for deadline.IsZero() || time.Now().Before(deadline) {
		db, err := open(addr, database)
		if err == nil {
			err = runOn(db, r, deadline)
			db.Close()
		}
		if err != nil {
			fmt.Printf("error %s: %s\n", time.Now().UTC().Format(time.RFC3339), err.Error())
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

func runOn(db *sql.DB, r *rand.Rand, deadline time.Time) error {
	ctx := context.Background()
	var next int64
	if err := db.QueryRowContext(ctx, `SELECT ISNULL(MAX(seq), -1) + 1 FROM ledger`).Scan(&next); err != nil {
		return err
	}
	fmt.Printf("start %d\n", next)
	for ; deadline.IsZero() || time.Now().Before(deadline); next++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		p := payload(next, 0, 64+int(next%448))
		if _, err := tx.Exec(`INSERT INTO ledger (seq, rev, payload, crc) VALUES (@p1, 0, @p2, @p3)`, next, p, checksum(p)); err != nil {
			tx.Rollback()
			return err
		}
		if next > 0 {
			old := r.Int64N(next)
			var rev int
			if err := tx.QueryRow(`SELECT rev FROM ledger WITH (UPDLOCK) WHERE seq = @p1`, old).Scan(&rev); err != nil {
				tx.Rollback()
				return fmt.Errorf("row %d below %d: %w", old, next, err)
			}
			q := payload(old, rev+1, 64+int((old+int64(rev)+1)%448))
			if _, err := tx.Exec(`UPDATE ledger SET rev = @p2, payload = @p3, crc = @p4 WHERE seq = @p1`, old, rev+1, q, checksum(q)); err != nil {
				tx.Rollback()
				return err
			}
		}
		if next%blobEvery == 0 {
			b := payload(next, -1, blobSize)
			if _, err := tx.Exec(`INSERT INTO blobs (seq, data, crc) VALUES (@p1, @p2, @p3)`, next, b, checksum(b)); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		fmt.Printf("ack %d\n", next)
	}
	return nil
}

func verify(addr string, acked int64) error {
	db, err := open(addr, database)
	if err != nil {
		return err
	}
	defer db.Close()
	var rows, newest int64
	if err := db.QueryRow(`SELECT COUNT_BIG(*), ISNULL(MAX(seq), -1) FROM ledger`).Scan(&rows, &newest); err != nil {
		return err
	}
	var problems []string
	if newest < acked {
		problems = append(problems, fmt.Sprintf("acknowledged seq %d lost: the newest row is %d", acked, newest))
	}
	if rows != newest+1 {
		problems = append(problems, fmt.Sprintf("%d rows for seqs 0..%d: gaps", rows, newest))
	}
	bad, err := badRows(db, `SELECT seq, payload, crc FROM ledger`)
	if err != nil {
		return err
	}
	problems = append(problems, bad...)
	if bad, err = badRows(db, `SELECT seq, data, crc FROM blobs`); err != nil {
		return err
	}
	problems = append(problems, bad...)
	if msgs, err := checkDB(db); err != nil {
		return err
	} else if len(msgs) > 0 {
		problems = append(problems, msgs...)
	}
	if len(problems) > 0 {
		if len(problems) > 10 {
			problems = append(problems[:10], fmt.Sprintf("... %d more", len(problems)-10))
		}
		return fmt.Errorf("%d problems:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	fmt.Printf("verified %d rows (newest %d, acknowledged %d), CHECKDB clean\n", rows, newest, acked)
	return nil
}

func badRows(db *sql.DB, query string) ([]string, error) {
	rs, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var bad []string
	for rs.Next() {
		var seq, crc int64
		var p []byte
		if err := rs.Scan(&seq, &p, &crc); err != nil {
			return nil, err
		}
		if checksum(p) != crc {
			bad = append(bad, fmt.Sprintf("seq %d: checksum mismatch", seq))
		}
	}
	return bad, rs.Err()
}

// checkDB runs DBCC CHECKDB; any message it returns is a problem.
func checkDB(db *sql.DB) ([]string, error) {
	rs, err := db.Query(`DBCC CHECKDB (soak) WITH NO_INFOMSGS, ALL_ERRORMSGS, TABLERESULTS`)
	if err != nil {
		return nil, fmt.Errorf("CHECKDB: %w", err)
	}
	defer rs.Close()
	cols, _ := rs.Columns()
	var msgs []string
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, c := range cols {
			if c == "MessageText" {
				msgs = append(msgs, fmt.Sprintf("CHECKDB: %v", vals[i]))
			}
		}
	}
	return msgs, rs.Err()
}
