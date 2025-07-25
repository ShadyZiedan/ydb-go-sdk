package main

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"time"

	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/retry"
	"github.com/ydb-platform/ydb-go-sdk/v3/retry/budget"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"

	"slo/internal/config"
	"slo/internal/generator"
)

const (
	createTemplate = `
		CREATE TABLE IF NOT EXISTS ` + "`%s`" + ` (
			hash              Uint64,
			id                Uint64,
			payload_str       Utf8,
			payload_double    Double,
			payload_timestamp Timestamp,
			payload_hash      Uint64,
			PRIMARY KEY (
				hash,
				id
			)
		) WITH (
			AUTO_PARTITIONING_BY_SIZE = ENABLED,
			AUTO_PARTITIONING_BY_LOAD = ENABLED,
			AUTO_PARTITIONING_PARTITION_SIZE_MB = %d,
			AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = %d,
			AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = %d,
			UNIFORM_PARTITIONS = %d
		);
	`
	dropTemplate = `
		DROP TABLE IF EXISTS ` + "`%s`" + `;
	`
	upsertTemplate = `
		UPSERT INTO ` + "`%s`" + ` (
			id, hash, payload_str, payload_double, payload_timestamp
		) VALUES (
			$id, Digest::NumericHash($id), $payload_str, $payload_double, $payload_timestamp
		);
	`
	selectTemplate = `
		SELECT id, payload_str, payload_double, payload_timestamp, payload_hash
		FROM ` + "`%s`" + ` WHERE id = $id AND hash = Digest::NumericHash($id);
	`
)

type Storage struct {
	cc          *ydb.Driver
	c           ydb.SQLConnector
	db          *sql.DB
	cfg         *config.Config
	createQuery string
	dropQuery   string
	upsertQuery string
	selectQuery string
	retryBudget interface {
		budget.Budget

		Stop()
	}
}

func NewStorage(ctx context.Context, cfg *config.Config, poolSize int) (s *Storage, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*5) //nolint:mnd
	defer cancel()

	retryBudget := budget.Limited(int(float64(poolSize) * 0.1)) //nolint:mnd

	s = &Storage{
		cfg: cfg,
		createQuery: fmt.Sprintf(createTemplate, cfg.Table,
			cfg.PartitionSize, cfg.MinPartitionsCount, cfg.MaxPartitionsCount, cfg.MinPartitionsCount),
		dropQuery:   fmt.Sprintf(dropTemplate, cfg.Table),
		upsertQuery: fmt.Sprintf(upsertTemplate, cfg.Table),
		selectQuery: fmt.Sprintf(selectTemplate, cfg.Table),
		retryBudget: retryBudget,
	}

	s.cc, err = ydb.Open(
		ctx,
		s.cfg.Endpoint+s.cfg.DB,
		ydb.WithRetryBudget(retryBudget),
	)
	if err != nil {
		return nil, fmt.Errorf("ydb.Open error: %w", err)
	}

	s.c, err = ydb.Connector(s.cc,
		ydb.WithAutoDeclare(),
		ydb.WithTablePathPrefix(path.Join(s.cc.Name(), label)),
		ydb.WithQueryService(true),
	)
	if err != nil {
		return nil, fmt.Errorf("ydb.Connector error: %w", err)
	}

	s.db = sql.OpenDB(s.c)

	s.db.SetMaxOpenConns(poolSize)
	s.db.SetMaxIdleConns(poolSize)
	s.db.SetConnMaxIdleTime(time.Second)

	return s, nil
}

func (s *Storage) Read(ctx context.Context, entryID generator.RowID) (res generator.Row, attempts int, err error) {
	if err = ctx.Err(); err != nil {
		return generator.Row{}, attempts, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.ReadTimeout)*time.Millisecond)
	defer cancel()

	err = retry.Do(ctx, s.db,
		func(ctx context.Context, cc *sql.Conn) (err error) {
			if err = ctx.Err(); err != nil {
				return err
			}

			row := cc.QueryRowContext(ctx, s.selectQuery,
				sql.Named("id", &entryID),
			)

			var hash *uint64

			return row.Scan(&res.ID, &res.PayloadStr, &res.PayloadDouble, &res.PayloadTimestamp, &hash)
		},
		retry.WithIdempotent(true),
		retry.WithTrace(
			&trace.Retry{
				OnRetry: func(info trace.RetryLoopStartInfo) func(trace.RetryLoopDoneInfo) {
					return func(info trace.RetryLoopDoneInfo) {
						attempts = info.Attempts
					}
				},
			},
		),
	)

	return res, attempts, err
}

func (s *Storage) Write(ctx context.Context, e generator.Row) (attempts int, err error) {
	if err = ctx.Err(); err != nil {
		return attempts, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.WriteTimeout)*time.Millisecond)
	defer cancel()

	err = retry.Do(ctx, s.db,
		func(ctx context.Context, cc *sql.Conn) (err error) {
			if err = ctx.Err(); err != nil {
				return err
			}

			_, err = cc.ExecContext(ctx, s.upsertQuery,
				sql.Named("id", e.ID),
				sql.Named("payload_str", *e.PayloadStr),
				sql.Named("payload_double", *e.PayloadDouble),
				sql.Named("payload_timestamp", *e.PayloadTimestamp),
			)

			return err
		},
		retry.WithIdempotent(true),
		retry.WithTrace(
			&trace.Retry{
				OnRetry: func(info trace.RetryLoopStartInfo) func(trace.RetryLoopDoneInfo) {
					return func(info trace.RetryLoopDoneInfo) {
						attempts = info.Attempts
					}
				},
			},
		),
	)

	return attempts, err
}

func (s *Storage) createTable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.WriteTimeout)*time.Millisecond)
	defer cancel()

	return retry.Do(ctx, s.db,
		func(ctx context.Context, cc *sql.Conn) error {
			_, err := s.db.ExecContext(ctx, s.createQuery)

			return err
		}, retry.WithIdempotent(true),
	)
}

func (s *Storage) dropTable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.WriteTimeout)*time.Millisecond)
	defer cancel()

	return retry.Do(ctx, s.db,
		func(ctx context.Context, cc *sql.Conn) error {
			_, err := s.db.ExecContext(ctx, s.dropQuery)

			return err
		}, retry.WithIdempotent(true),
	)
}

func (s *Storage) close(ctx context.Context) error {
	s.retryBudget.Stop()

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := s.db.Close(); err != nil {
		return fmt.Errorf("error close database/sql driver: %w", err)
	}

	if err := s.c.Close(); err != nil {
		return fmt.Errorf("error close connector: %w", err)
	}

	if err := s.cc.Close(ctx); err != nil {
		return fmt.Errorf("error close ydb driver: %w", err)
	}

	return nil
}
