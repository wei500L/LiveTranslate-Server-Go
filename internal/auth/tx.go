package auth

import (
	"github.com/jackc/pgx/v5"
)

// pgxTx aliases pgx.Tx so service.go reads cleanly.
type pgxTx = pgx.Tx
