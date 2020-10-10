package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	xsuportal "github.com/isucon/isucon10-final/webapp/golang"
	"github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/resources"
	"github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/services/bench"
	"github.com/isucon/isucon10-final/webapp/golang/util"
)

var db *sqlx.DB

var benchmarkJobIdChannel chan (int)

type benchmarkQueueService struct {
}

func (b *benchmarkQueueService) Svc() *bench.BenchmarkQueueService {
	return &bench.BenchmarkQueueService{
		ReceiveBenchmarkJob: b.ReceiveBenchmarkJob,
	}
}

func (b *benchmarkQueueService) ReceiveBenchmarkJob(ctx context.Context, req *bench.ReceiveBenchmarkJobRequest) (*bench.ReceiveBenchmarkJobResponse, error) {
	var jobHandle *bench.ReceiveBenchmarkJobResponse_JobHandle
	for {
		next, err := func() (bool, error) {
			job, err := pollBenchmarkJob(db)
			if err != nil {
				return false, fmt.Errorf("poll benchmark job: %w", err)
			}
			if job == nil {
				return false, nil
			}

			randomBytes := make([]byte, 16)
			_, err = rand.Read(randomBytes)
			if err != nil {
				return false, fmt.Errorf("read random: %w", err)
			}
			handle := base64.StdEncoding.EncodeToString(randomBytes)
			result, err := db.Exec(
				"UPDATE `benchmark_jobs` SET `status` = ?, `handle` = ? WHERE `id` = ? AND `status` = ?",
				resources.BenchmarkJob_SENT,
				handle,
				job.ID,
				resources.BenchmarkJob_PENDING,
			)
			if err != nil {
				return false, fmt.Errorf("update benchmark job status: %w", err)
			}
			rowsAffected, err := result.RowsAffected()
			if err != nil {
				return false, fmt.Errorf("update benchmark job status: %w", err)
			}
			if rowsAffected == 0 {
				return true, nil
			}

			jobHandle = &bench.ReceiveBenchmarkJobResponse_JobHandle{
				JobId:            job.ID,
				Handle:           handle,
				TargetHostname:   job.TargetHostName,
				ContestStartedAt: timestamppb.New(contestStartsAt),
				JobCreatedAt:     timestamppb.New(job.CreatedAt),
			}
			return false, nil
		}()
		if err != nil {
			return nil, fmt.Errorf("fetch queue: %w", err)
		}
		if !next {
			break
		}
	}
	if jobHandle != nil {
		//log.Printf("[DEBUG] Dequeued: job_handle=%+v", jobHandle)
	}
	return &bench.ReceiveBenchmarkJobResponse{
		JobHandle: jobHandle,
	}, nil
}

type benchmarkReportService struct {
}

func (b *benchmarkReportService) Svc() *bench.BenchmarkReportService {
	return &bench.BenchmarkReportService{
		ReportBenchmarkResult: b.ReportBenchmarkResult,
	}
}

func (b *benchmarkReportService) ReportBenchmarkResult(srv bench.BenchmarkReport_ReportBenchmarkResultServer) error {
	for {
		req, err := srv.Recv()
		if err != nil {
			return err
		}
		if req.Result == nil {
			return status.Error(codes.InvalidArgument, "result required")
		}

		err = func() error {
			tx, err := db.Beginx()
			if err != nil {
				return fmt.Errorf("begin tx: %w", err)
			}
			defer tx.Rollback()

			var job xsuportal.BenchmarkJob
			err = tx.Get(
				&job,
				"SELECT * FROM `benchmark_jobs` WHERE `id` = ? AND `handle` = ? FOR UPDATE",
				req.JobId,
				req.Handle,
			)
			if err == sql.ErrNoRows {
				//log.Printf("[ERROR] Job not found: job_id=%v, handle=%+v", req.JobId, req.Handle)
				return status.Errorf(codes.NotFound, "Job %d not found or handle is wrong", req.JobId)
			}
			if err != nil {
				return fmt.Errorf("get benchmark job: %w", err)
			}
			if req.Result.Finished {
				//log.Printf("[DEBUG] %v: save as finished", req.JobId)
				if err := b.saveAsFinished(tx, &job, req); err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return fmt.Errorf("commit tx: %w", err)
				}
				if err := xsuportal.NotifyBenchmarkJobFinished(db, &job); err != nil {
					return fmt.Errorf("notify benchmark job finished: %w", err)
				}
			} else {
				//log.Printf("[DEBUG] %v: save as running", req.JobId)
				if err := b.saveAsRunning(tx, &job, req); err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return fmt.Errorf("commit tx: %w", err)
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
		err = srv.Send(&bench.ReportBenchmarkResultResponse{
			AckedNonce: req.GetNonce(),
		})
		if err != nil {
			return fmt.Errorf("send report: %w", err)
		}
	}
}

func (b *benchmarkReportService) saveAsFinished(db *sqlx.Tx, job *xsuportal.BenchmarkJob, req *bench.ReportBenchmarkResultRequest) error {
	if !job.StartedAt.Valid || job.FinishedAt.Valid {
		return status.Errorf(codes.FailedPrecondition, "Job %v has already finished or has not started yet", req.JobId)
	}
	if req.Result.MarkedAt == nil {
		return status.Errorf(codes.InvalidArgument, "marked_at is required")
	}
	markedAt := req.Result.MarkedAt.AsTime().Round(time.Microsecond)

	result := req.Result
	var raw, deduction, full sql.NullInt32
	if result.ScoreBreakdown != nil {
		raw.Valid = true
		raw.Int32 = int32(result.ScoreBreakdown.Raw)
		deduction.Valid = true
		deduction.Int32 = int32(result.ScoreBreakdown.Deduction)
		full.Valid = true
		full.Int32 = int32(result.ScoreBreakdown.Raw) - int32(result.ScoreBreakdown.Deduction)
	}
	_, err := db.Exec(
		"UPDATE `benchmark_jobs` SET `status` = ?, `score_raw` = ?, `score_deduction` = ?, `passed` = ?, `reason` = ?, `updated_at` = NOW(6), `finished_at` = ? WHERE `id` = ?",
		resources.BenchmarkJob_FINISHED,
		raw,
		deduction,
		result.Passed,
		result.Reason,
		markedAt,
		req.JobId,
	)
	if err != nil {
		return fmt.Errorf("update benchmark job status: %w", err)
	}
	q := "UPDATE `team_scores`" +
		"SET " +
		"`best_score` = IF(? >= IFNULL(`best_score`,0),?,`best_score`), " +
		"`best_started_at` = IF(? >= IFNULL(`best_score`,0),?,`best_started_at`), " +
		"`best_finished_at` = IF(? >= IFNULL(`best_score`,0),?,`best_finished_at`), " +
		"`latest_score` = ?, " +
		"`latest_started_at` = ?, " +
		"`latest_finished_at` = ?, " +
		"`finish_count` = IFNULL(`finish_count`,0) + 1 "
	args := []interface{}{full, full, full, job.StartedAt, full, markedAt, full, job.StartedAt, markedAt}
	if markedAt.After(contestFreezesAt) || markedAt.Equal(contestFreezesAt) {
		q = q +
			", `fz_best_score` = IF(? >= IFNULL(`fz_best_score`,0),?,`fz_best_score`), " +
			"`fz_best_started_at` = IF(? >= IFNULL(`fz_best_score`,0),?,`fz_best_started_at`), " +
			"`fz_best_finished_at` = IF(? >= IFNULL(`fz_best_score`,0),?,`fz_best_finished_at`), " +
			"`fz_latest_score` = ?, " +
			"`fz_latest_started_at` = ?, " +
			"`fz_latest_finished_at` = ?, " +
			"`fz_finish_count` = IFNULL(`fz_finish_count`,0) + 1 "
		args = append(args, full, full, full, job.StartedAt, full, markedAt, full, job.StartedAt, markedAt)
	}
	q = q + "WHERE `team_id` = ?"
	args = append(args, job.TeamID)
	_, err = db.Exec(q, args...)
	if err != nil {
		return fmt.Errorf("update benchmark job status: %w", err)
	}

	return nil
}

func (b *benchmarkReportService) saveAsRunning(db sqlx.Execer, job *xsuportal.BenchmarkJob, req *bench.ReportBenchmarkResultRequest) error {
	if req.Result.MarkedAt == nil {
		return status.Errorf(codes.InvalidArgument, "marked_at is required")
	}
	var startedAt time.Time
	if job.StartedAt.Valid {
		startedAt = job.StartedAt.Time
	} else {
		startedAt = req.Result.MarkedAt.AsTime().Round(time.Microsecond)
	}
	_, err := db.Exec(
		"UPDATE `benchmark_jobs` SET `status` = ?, `score_raw` = NULL, `score_deduction` = NULL, `passed` = FALSE, `reason` = NULL, `started_at` = ?, `updated_at` = NOW(6), `finished_at` = NULL WHERE `id` = ?",
		resources.BenchmarkJob_RUNNING,
		startedAt,
		req.JobId,
	)
	if err != nil {
		return fmt.Errorf("update benchmark job status: %w", err)
	}
	return nil
}

func pollBenchmarkJob(db sqlx.Queryer) (*xsuportal.BenchmarkJob, error) {
	select {
	case id := <-benchmarkJobIdChannel:
		var job xsuportal.BenchmarkJob
		err := sqlx.Get(
			db,
			&job,
			"SELECT * FROM `benchmark_jobs` WHERE `status` = ? AND id = ?",
			resources.BenchmarkJob_PENDING, id,
		)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("get benchmark job: %w", err)
		}
		return &job, nil
	case <-time.After(500 * time.Millisecond):
		return nil, nil
	}
}

func pollBenchmarkJobOld(db sqlx.Queryer) (*xsuportal.BenchmarkJob, error) {
	for i := 0; i < 10; i++ {
		if i >= 1 {
			time.Sleep(50 * time.Millisecond)
		}
		var job xsuportal.BenchmarkJob
		err := sqlx.Get(
			db,
			&job,
			"SELECT * FROM `benchmark_jobs` WHERE `status` = ? ORDER BY `id` LIMIT 1",
			resources.BenchmarkJob_PENDING,
		)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get benchmark job: %w", err)
		}
		return &job, nil
	}
	return nil, nil
}

func enqueueBenchmarkJob(e echo.Context) error {
	id, err := strconv.Atoi(e.Param("id"))
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	benchmarkJobIdChannel <- id
	return e.JSON(http.StatusOK, nil)
}

var contestStartsAt time.Time
var contestFreezesAt time.Time

func backgroundLeaderboardPB() {
	ticker := time.NewTicker(1000 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			var contestStartsAt time.Time
			_ = db.Get(&contestStartsAt, "SELECT `contest_starts_at` FROM `contest_config`")
			_ = db.Get(&contestFreezesAt, "SELECT `contest_freezes_at` FROM `contest_config`")
		}
	}
}

func main() {
	port := util.GetEnv("PORT", "50051")
	address := ":" + port

	listener, err := net.Listen("tcp", address)
	if err != nil {
		panic(err)
	}
	log.Print("[INFO] listen ", address)

	db, _ = xsuportal.GetDB()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(20)

	benchmarkJobIdChannel = make(chan int, 300*4) // xsuportal.TeamCapacity
	srv := echo.New()
	srv.POST("/api/contestant/benchmark_jobs/:id", enqueueBenchmarkJob)
	go func() {
		srv.Start(":60051")
	}()

	xsuportal.PreLoadVAPIDKey()

	server := grpc.NewServer()

	queue := &benchmarkQueueService{}
	report := &benchmarkReportService{}

	bench.RegisterBenchmarkQueueService(server, queue.Svc())
	bench.RegisterBenchmarkReportService(server, report.Svc())

	if err := server.Serve(listener); err != nil {
		panic(err)
	}
}
