package main

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/nakagami/firebirdsql"
)

func main() {
	db, _ := sql.Open("firebirdsql", "sysdba:masterkey@localhost:3055/C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb?charset=UTF8")
	defer db.Close()
	ctx := context.Background()
	db.ExecContext(ctx, "DROP TABLE W_TEST")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fmt.Println("begin:", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "CREATE TABLE W_TEST (ID INT, VAL VARCHAR(10))"); err != nil {
		fmt.Println("create:", err)
		return
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO W_TEST VALUES (1, 'a')"); err != nil {
		fmt.Println("insert:", err)
		return
	}
	if err := tx.Commit(); err != nil {
		fmt.Println("commit:", err)
		return
	}
	fmt.Println("single-tx DDL+DML OK")
	db.ExecContext(ctx, "DROP TABLE W_TEST")
}
