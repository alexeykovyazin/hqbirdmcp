// P0.1 connectivity spike — driver viability against local FB 2.5/3/4/5.
// Throwaway code: hardcoded ports and masterkey are acceptable here only.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/nakagami/firebirdsql"
)

type inst struct {
	name string
	addr string
}

var instances = []inst{
	{"FB2.5", "localhost:3052"},
	{"FB3.0", "localhost:3053"},
	{"FB4.0", "localhost:3054"},
	{"FB5.0", "localhost:3055"},
}

func main() {
	only := ""
	if len(os.Args) > 1 {
		only = os.Args[1]
	}
	for _, in := range instances {
		if only != "" && only != in.name {
			continue
		}
		fmt.Printf("=== %s (%s)\n", in.name, in.addr)
		dbFile := fmt.Sprintf(`C:/HQbirdData/output/fbmcp-spike/spike_%s.fdb`, in.name)
		db, err := sql.Open("firebirdsql", dsn(in, dbFile))
		if err != nil {
			fmt.Printf("[%s] open err: %v\n", in.name, err)
			continue
		}
		checkBasic(in, db)
		checkMon(in, db)
		checkReadOnly(in, db)
		fmt.Printf("[%s] plan: no driver-native plan API (finding: isql subprocess fallback for P2.4)\n", in.name)
		db.Close()
	}
}

func checkBasic(in inst, db *sql.DB) {
	var version string
	err := db.QueryRow("SELECT RDB$ENGINE_VERSION FROM RDB$DATABASE").Scan(&version)
	if err != nil {
		err = db.QueryRow("SELECT MON$SERVER_VERSION FROM MON$DATABASE").Scan(&version)
	}
	if err != nil {
		fmt.Printf("[%s] basic: FAIL %v\n", in.name, firstLine(err.Error()))
		return
	}
	fmt.Printf("[%s] basic: OK engine=%s\n", in.name, version)
}

func checkMon(in inst, db *sql.DB) {
	var n int
	for _, q := range []string{
		"SELECT COUNT(*) FROM MON$DATABASE",
		"SELECT COUNT(*) FROM MON$ATTACHMENTS",
		"SELECT COUNT(*) FROM MON$TRANSACTIONS",
		"SELECT COUNT(*) FROM MON$STATEMENTS",
		"SELECT COUNT(*) FROM MON$IO_STATS",
		"SELECT COUNT(*) FROM MON$RECORD_STATS",
	} {
		if err := db.QueryRow(q).Scan(&n); err != nil {
			fmt.Printf("[%s] MON$: %-26s FAIL %s\n", in.name, q[20:], firstLine(err.Error()))
		} else {
			fmt.Printf("[%s] MON$: %-26s OK rows=%d\n", in.name, q[20:], n)
		}
	}
}

// The core proof: a read-only transaction must make the ENGINE refuse writes.
func checkReadOnly(in inst, db *sql.DB) {
	if _, err := db.Exec("CREATE TABLE SPIKE_RO (ID INT, VAL VARCHAR(20))"); err == nil {
		db.Exec("DROP TABLE SPIKE_RO2")
	} else {
		fmt.Printf("[%s] RO setup: %s\n", in.name, firstLine(err.Error()))
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		fmt.Printf("[%s] RO begin-tx: FAIL %v\n", in.name, firstLine(err.Error()))
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO SPIKE_RO (ID, VAL) VALUES (1, 'X')"); err != nil {
		fmt.Printf("[%s] RO refusal (DML): OK engine-refused: %s\n", in.name, firstLine(err.Error()))
	} else {
		fmt.Printf("[%s] RO refusal (DML): **FAIL** — write succeeded in read-only tx!\n", in.name)
	}
	if _, err := tx.Exec("CREATE TABLE SPIKE_RO2 (ID INT)"); err != nil {
		fmt.Printf("[%s] RO refusal (DDL): OK engine-refused: %s\n", in.name, firstLine(err.Error()))
	} else {
		fmt.Printf("[%s] RO refusal (DDL): **FAIL** — DDL succeeded in read-only tx!\n", in.name)
	}
}

func dsn(in inst, dbfile string) string {
	return fmt.Sprintf("sysdba:masterkey@%s/%s?charset=UTF8", in.addr, dbfile)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	if len(s) > 140 {
		return s[:140]
	}
	return s
}
