package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"google.golang.org/protobuf/types/known/timestamppb"

	"golang.org/x/sync/singleflight"

	xsuportal "github.com/isucon/isucon10-final/webapp/golang"
	xsuportalpb "github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal"
	resourcespb "github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/resources"
	adminpb "github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/services/admin"
	audiencepb "github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/services/audience"
	commonpb "github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/services/common"
	contestantpb "github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/services/contestant"
	registrationpb "github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/services/registration"
	"github.com/isucon/isucon10-final/webapp/golang/util"
)

const (
	TeamCapacity               = 110
	AdminID                    = "admin"
	AdminPassword              = "admin"
	DebugContestStatusFilePath = "/tmp/XSUPORTAL_CONTEST_STATUS"
	MYSQL_ER_DUP_ENTRY         = 1062
	SessionName                = "xsucon_session"
)

var db *sqlx.DB
var sfGroup singleflight.Group

/*
func StaticHeader(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		path := c.Request().URL.Path
		if path == "/favicon.ico" || strings.HasPrefix(path, "/packs/") {
			c.Response().Header().Set("Cache-Control", "max-age=3600")
		}
		return next(c)
	}
}
*/

func main() {
	srv := echo.New()
	srv.Debug = false
	srv.Server.Addr = fmt.Sprintf(":%v", util.GetEnv("PORT", "9292"))
	srv.HideBanner = true

	srv.Binder = ProtoBinder{}
	srv.HTTPErrorHandler = func(err error, c echo.Context) {
		if !c.Response().Committed {
			c.Logger().Error(c.Request().Method, " ", c.Request().URL.Path, " ", err)
			_ = halt(c, http.StatusInternalServerError, "", err)
		}
	}

	xsuportal.PreLoadVAPIDKey()

	db, _ = xsuportal.GetDB()
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(50)

	//srv.Use(middleware.Logger())
	srv.Use(middleware.Recover())
	srv.Use(session.Middleware(sessions.NewCookieStore([]byte("tagomoris"))))
	// srv.Use(StaticHeader)
	// srv.Use(middleware.Gzip())

	srv.File("/", "public/audience.html")
	srv.File("/registration", "public/audience.html")
	srv.File("/signup", "public/audience.html")
	srv.File("/login", "public/audience.html")
	srv.File("/logout", "public/audience.html")
	srv.File("/teams", "public/audience.html")

	srv.File("/contestant", "public/contestant.html")
	srv.File("/contestant/benchmark_jobs", "public/contestant.html")
	srv.File("/contestant/benchmark_jobs/:id", "public/contestant.html")
	srv.File("/contestant/clarifications", "public/contestant.html")

	srv.File("/admin", "public/admin.html")
	srv.File("/admin/", "public/admin.html")
	srv.File("/admin/clarifications", "public/admin.html")
	srv.File("/admin/clarifications/:id", "public/admin.html")

	srv.Static("/", "public")

	admin := &AdminService{}
	audience := &AudienceService{}
	registration := &RegistrationService{}
	contestant := &ContestantService{}
	common := &CommonService{}

	srv.POST("/initialize", admin.Initialize)
	srv.GET("/api/admin/clarifications", admin.ListClarifications)
	srv.GET("/api/admin/clarifications/:id", admin.GetClarification)
	srv.PUT("/api/admin/clarifications/:id", admin.RespondClarification)
	srv.GET("/api/session", common.GetCurrentSession)
	srv.GET("/api/audience/teams", audience.ListTeams)
	srv.GET("/api/audience/dashboard", audience.Dashboard)
	srv.GET("/api/registration/session", registration.GetRegistrationSession)
	srv.POST("/api/registration/team", registration.CreateTeam)
	srv.POST("/api/registration/contestant", registration.JoinTeam)
	srv.PUT("/api/registration", registration.UpdateRegistration)
	srv.DELETE("/api/registration", registration.DeleteRegistration)
	srv.POST("/api/contestant/benchmark_jobs", contestant.EnqueueBenchmarkJob)
	srv.GET("/api/contestant/benchmark_jobs", contestant.ListBenchmarkJobs)
	srv.GET("/api/contestant/benchmark_jobs/:id", contestant.GetBenchmarkJob)
	srv.GET("/api/contestant/clarifications", contestant.ListClarifications)
	srv.POST("/api/contestant/clarifications", contestant.RequestClarification)
	srv.GET("/api/contestant/dashboard", contestant.Dashboard)
	srv.GET("/api/contestant/notifications", contestant.ListNotifications)
	srv.POST("/api/contestant/push_subscriptions", contestant.SubscribeNotification)
	srv.DELETE("/api/contestant/push_subscriptions", contestant.UnsubscribeNotification)
	srv.POST("/api/signup", contestant.Signup)
	srv.POST("/api/login", contestant.Login)
	srv.POST("/api/logout", contestant.Logout)

	go func() { backgroundLeaderboardPB() }()

	srv.Logger.Error(srv.StartServer(srv.Server))
}

type ProtoBinder struct{}

func (p ProtoBinder) Bind(i interface{}, e echo.Context) error {
	rc := e.Request().Body
	defer rc.Close()
	b, err := ioutil.ReadAll(rc)
	if err != nil {
		return halt(e, http.StatusBadRequest, "", fmt.Errorf("read request body: %w", err))
	}
	if err := proto.Unmarshal(b, i.(proto.Message)); err != nil {
		return halt(e, http.StatusBadRequest, "", fmt.Errorf("unmarshal request body: %w", err))
	}
	return nil
}

type AdminService struct{}

func (*AdminService) Initialize(e echo.Context) error {
	var req adminpb.InitializeRequest
	if err := e.Bind(&req); err != nil {
		return err
	}

	queries := []string{
		"TRUNCATE `teams`",
		"TRUNCATE `contestants`",
		"TRUNCATE `benchmark_jobs`",
		"TRUNCATE `clarifications`",
		"TRUNCATE `notifications`",
		"TRUNCATE `push_subscriptions`",
		"TRUNCATE `contest_config`",
	}
	for _, query := range queries {
		_, err := db.Exec(query)
		if err != nil {
			return fmt.Errorf("truncate table: %w", err)
		}
	}

	pushSubCacheLock.Lock()
	pushSubCache = make(map[string]int)
	pushSubCacheLock.Unlock()

	passwordHash := sha256.Sum256([]byte(AdminPassword))
	digest := hex.EncodeToString(passwordHash[:])
	_, err := db.Exec("INSERT `contestants` (`id`, `password`, `staff`, `created_at`) VALUES (?, ?, TRUE, NOW(6))", AdminID, digest)
	if err != nil {
		return fmt.Errorf("insert initial contestant: %w", err)
	}

	if req.Contest != nil {
		_, err := db.Exec(
			"INSERT `contest_config` (`registration_open_at`, `contest_starts_at`, `contest_freezes_at`, `contest_ends_at`) VALUES (?, ?, ?, ?)",
			req.Contest.RegistrationOpenAt.AsTime().Round(time.Microsecond),
			req.Contest.ContestStartsAt.AsTime().Round(time.Microsecond),
			req.Contest.ContestFreezesAt.AsTime().Round(time.Microsecond),
			req.Contest.ContestEndsAt.AsTime().Round(time.Microsecond),
		)
		if err != nil {
			return fmt.Errorf("insert contest: %w", err)
		}
	} else {
		_, err := db.Exec("INSERT `contest_config` (`registration_open_at`, `contest_starts_at`, `contest_freezes_at`, `contest_ends_at`) VALUES (TIMESTAMPADD(SECOND, 0, NOW(6)), TIMESTAMPADD(SECOND, 5, NOW(6)), TIMESTAMPADD(SECOND, 40, NOW(6)), TIMESTAMPADD(SECOND, 50, NOW(6)))")
		if err != nil {
			return fmt.Errorf("insert contest: %w", err)
		}
	}

	contestStatusCache = nil

	host := util.GetEnv("BENCHMARK_SERVER_HOST", "localhost")
	port, _ := strconv.Atoi(util.GetEnv("BENCHMARK_SERVER_PORT", "50051"))
	res := &adminpb.InitializeResponse{
		Language: "go",
		BenchmarkServer: &adminpb.InitializeResponse_BenchmarkServer{
			Host: host,
			Port: int64(port),
		},
	}
	return writeProto(e, http.StatusOK, res)
}

func (*AdminService) ListClarifications(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{}); !ok {
		return wrapError("check session", err)
	}
	contestant, _ := getCurrentContestant(e, db, false)
	if !contestant.Staff {
		return halt(e, http.StatusForbidden, "管理者権限が必要です", nil)
	}
	var clarifications []xsuportal.Clarification
	err := db.Select(&clarifications, "SELECT * FROM `clarifications` ORDER BY `updated_at` DESC")
	if err != sql.ErrNoRows && err != nil {
		return fmt.Errorf("query clarifications: %w", err)
	}
	teamsH := make(map[int64]xsuportal.Team, 0)
	teamsL := make([]xsuportal.Team, 0)
	_ = db.Select(
		&teamsL,
		"SELECT * FROM `teams`",
	)
	for _, t := range teamsL {
		teamsH[t.ID] = t
	}
	res := &adminpb.ListClarificationsResponse{}
	for _, clarification := range clarifications {
		var team xsuportal.Team
		var ok bool
		if team, ok = teamsH[clarification.TeamID]; !ok {
			return fmt.Errorf("query team(id=%v, clarification=%v): %w", clarification.TeamID, clarification.ID, err)
		}
		c, err := makeClarificationPB(db, &clarification, &team)
		if err != nil {
			return fmt.Errorf("make clarification: %w", err)
		}
		res.Clarifications = append(res.Clarifications, c)
	}
	return writeProto(e, http.StatusOK, res)
}

func (*AdminService) GetClarification(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{}); !ok {
		return wrapError("check session", err)
	}
	id, err := strconv.Atoi(e.Param("id"))
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	contestant, _ := getCurrentContestant(e, db, false)
	if !contestant.Staff {
		return halt(e, http.StatusForbidden, "管理者権限が必要です", nil)
	}
	var clarification xsuportal.Clarification
	err = db.Get(
		&clarification,
		"SELECT * FROM `clarifications` WHERE `id` = ?",
		id,
	)
	if err != nil {
		return fmt.Errorf("get clarification: %w", err)
	}
	var team xsuportal.Team
	err = db.Get(
		&team,
		"SELECT * FROM `teams` WHERE id = ?",
		clarification.TeamID,
	)
	if err != nil {
		return fmt.Errorf("get team: %w", err)
	}
	c, err := makeClarificationPB(db, &clarification, &team)
	if err != nil {
		return fmt.Errorf("make clarification: %w", err)
	}
	return writeProto(e, http.StatusOK, &adminpb.GetClarificationResponse{
		Clarification: c,
	})
}

func (*AdminService) RespondClarification(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{}); !ok {
		return wrapError("check session", err)
	}
	id, err := strconv.Atoi(e.Param("id"))
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	contestant, _ := getCurrentContestant(e, db, false)
	if !contestant.Staff {
		return halt(e, http.StatusForbidden, "管理者権限が必要です", nil)
	}
	var req adminpb.RespondClarificationRequest
	if err := e.Bind(&req); err != nil {
		return err
	}

	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var clarificationBefore xsuportal.Clarification
	err = tx.Get(
		&clarificationBefore,
		"SELECT * FROM `clarifications` WHERE `id` = ? FOR UPDATE",
		id,
	)
	if err == sql.ErrNoRows {
		return halt(e, http.StatusNotFound, "質問が見つかりません", nil)
	}
	if err != nil {
		return fmt.Errorf("get clarification with lock: %w", err)
	}
	wasAnswered := clarificationBefore.AnsweredAt.Valid
	wasDisclosed := clarificationBefore.Disclosed

	_, err = tx.Exec(
		"UPDATE `clarifications` SET `disclosed` = ?, `answer` = ?, `updated_at` = NOW(6), `answered_at` = NOW(6) WHERE `id` = ?",
		req.Disclose,
		req.Answer,
		id,
	)
	if err != nil {
		return fmt.Errorf("update clarification: %w", err)
	}
	var clarification xsuportal.Clarification
	err = tx.Get(
		&clarification,
		"SELECT * FROM `clarifications` WHERE `id` = ?",
		id,
	)
	if err != nil {
		return fmt.Errorf("get clarification: %w", err)
	}
	var team xsuportal.Team
	err = tx.Get(
		&team,
		"SELECT * FROM `teams` WHERE `id` = ?",
		clarification.TeamID,
	)
	if err != nil {
		return fmt.Errorf("get team: %w", err)
	}
	c, err := makeClarificationPB(tx, &clarification, &team)
	if err != nil {
		return fmt.Errorf("make clarification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	updated := wasAnswered && wasDisclosed == clarification.Disclosed
	if err := xsuportal.NotifyClarificationAnswered(db, &clarification, updated); err != nil {
		return fmt.Errorf("notify clarification answered: %w", err)
	}
	return writeProto(e, http.StatusOK, &adminpb.RespondClarificationResponse{
		Clarification: c,
	})
}

type CommonService struct{}

func (*CommonService) GetCurrentSession(e echo.Context) error {
	res := &commonpb.GetCurrentSessionResponse{}
	currentContestant, err := getCurrentContestant(e, db, false)
	if err != nil {
		return fmt.Errorf("get current contestant: %w", err)
	}
	if currentContestant != nil {
		res.Contestant = makeContestantPB(currentContestant)
	}
	currentTeam, err := getCurrentTeam(e, db, false)
	if err != nil {
		return fmt.Errorf("get current team: %w", err)
	}
	if currentTeam != nil {
		res.Team, err = makeTeamPB(db, currentTeam, true, true)
		if err != nil {
			return fmt.Errorf("make team: %w", err)
		}
	}
	res.Contest, err = makeContestPB(e)
	if err != nil {
		return fmt.Errorf("make contest: %w", err)
	}
	vapidKey := xsuportal.VAPIDKey()
	if vapidKey != nil {
		res.PushVapidKey = vapidKey.VAPIDPublicKey
	}
	return writeProto(e, http.StatusOK, res)
}

type ContestantService struct{}

func (*ContestantService) EnqueueBenchmarkJob(e echo.Context) error {
	var req contestantpb.EnqueueBenchmarkJobRequest
	if err := e.Bind(&req); err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if ok, err := loginRequired(e, tx, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}
	if ok, err := contestStatusRestricted(e, tx, resourcespb.Contest_STARTED, "競技時間外はベンチマークを実行できません"); !ok {
		return wrapError("check contest status", err)
	}
	team, _ := getCurrentTeam(e, tx, false)
	var jobCount int
	err = tx.Get(
		&jobCount,
		"SELECT COUNT(*) AS `cnt` FROM `benchmark_jobs` WHERE `team_id` = ? AND `finished_at` IS NULL",
		team.ID,
	)
	if err != nil {
		return fmt.Errorf("count benchmark job: %w", err)
	}
	if jobCount > 0 {
		return halt(e, http.StatusForbidden, "既にベンチマークを実行中です", nil)
	}
	_, err = tx.Exec(
		"INSERT INTO `benchmark_jobs` (`team_id`, `target_hostname`, `status`, `updated_at`, `created_at`) VALUES (?, ?, ?, NOW(6), NOW(6))",
		team.ID,
		req.TargetHostname,
		int(resourcespb.BenchmarkJob_PENDING),
	)
	if err != nil {
		return fmt.Errorf("enqueue benchmark job: %w", err)
	}
	var job xsuportal.BenchmarkJob
	err = tx.Get(
		&job,
		"SELECT * FROM `benchmark_jobs` WHERE `id` = (SELECT LAST_INSERT_ID())",
	)
	if err != nil {
		return fmt.Errorf("get benchmark job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	j := makeBenchmarkJobPB(&job)

	//
	url_target := "http://localhost:60051/api/contestant/benchmark_jobs"
	args := url.Values{}
	res, err := http.PostForm(url_target, args)
	defer res.Body.Close()
	if err != nil {
		return fmt.Errorf("pass job id to benchmark: %w", err)
	}
	//
	return writeProto(e, http.StatusOK, &contestantpb.EnqueueBenchmarkJobResponse{
		Job: j,
	})
}

func (*ContestantService) ListBenchmarkJobs(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}
	team, _ := getCurrentTeam(e, db, false)
	res, err, _ := sfGroup.Do(fmt.Sprintf("listbench%d", team.ID), func() (interface{}, error) {
		job, err := makeBenchmarkJobsPB(e, db, 0)
		if err != nil {
			return nil, fmt.Errorf("make benchmark jobs: %w", err)
		}
		r := &contestantpb.ListBenchmarkJobsResponse{Jobs: job}
		return proto.Marshal(r)
	})
	if err != nil {
		return err
	}
	return e.Blob(http.StatusOK, "application/vnd.google.protobuf", res.([]byte))
}

func (*ContestantService) GetBenchmarkJob(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}
	id, err := strconv.Atoi(e.Param("id"))
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	team, _ := getCurrentTeam(e, db, false)
	var job xsuportal.BenchmarkJob
	err = db.Get(
		&job,
		"SELECT * FROM `benchmark_jobs` WHERE `team_id` = ? AND `id` = ?",
		team.ID,
		id,
	)
	if err == sql.ErrNoRows {
		return halt(e, http.StatusNotFound, "ベンチマークジョブが見つかりません", nil)
	}
	if err != nil {
		return fmt.Errorf("get benchmark job: %w", err)
	}
	return writeProto(e, http.StatusOK, &contestantpb.GetBenchmarkJobResponse{
		Job: makeBenchmarkJobPB(&job),
	})
}

func (*ContestantService) ListClarifications(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}
	team, _ := getCurrentTeam(e, db, false)
	var clarifications []xsuportal.Clarification
	err := db.Select(
		&clarifications,
		"SELECT * FROM `clarifications` WHERE `team_id` = ? OR `disclosed` = TRUE ORDER BY `id` DESC",
		team.ID,
	)
	if err != sql.ErrNoRows && err != nil {
		return fmt.Errorf("select clarifications: %w", err)
	}
	res := &contestantpb.ListClarificationsResponse{}
	for _, clarification := range clarifications {
		var team xsuportal.Team
		err := db.Get(
			&team,
			"SELECT * FROM `teams` WHERE `id` = ?",
			clarification.TeamID,
		)
		if err != nil {
			return fmt.Errorf("get team(id=%v): %w", clarification.TeamID, err)
		}
		c, err := makeClarificationPB(db, &clarification, &team)
		if err != nil {
			return fmt.Errorf("make clarification: %w", err)
		}
		res.Clarifications = append(res.Clarifications, c)
	}
	return writeProto(e, http.StatusOK, res)
}

func (*ContestantService) RequestClarification(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}
	var req contestantpb.RequestClarificationRequest
	if err := e.Bind(&req); err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	team, _ := getCurrentTeam(e, tx, false)
	_, err = tx.Exec(
		"INSERT INTO `clarifications` (`team_id`, `question`, `created_at`, `updated_at`) VALUES (?, ?, NOW(6), NOW(6))",
		team.ID,
		req.Question,
	)
	if err != nil {
		return fmt.Errorf("insert clarification: %w", err)
	}
	var clarification xsuportal.Clarification
	err = tx.Get(&clarification, "SELECT * FROM `clarifications` WHERE `id` = LAST_INSERT_ID()")
	if err != nil {
		return fmt.Errorf("get clarification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	c, err := makeClarificationPB(db, &clarification, team)
	if err != nil {
		return fmt.Errorf("make clarification: %w", err)
	}
	return writeProto(e, http.StatusOK, &contestantpb.RequestClarificationResponse{
		Clarification: c,
	})
}

func (*ContestantService) Dashboard(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}
	team, _ := getCurrentTeam(e, db, false)
	res, err, _ := sfGroup.Do(fmt.Sprintf("dashboard%d", team.ID), func() (interface{}, error) {
		leaderboard, err := makeLeaderboardPB(team.ID)
		if err != nil {
			return nil, fmt.Errorf("make leaderboard: %w", err)
		}
		r := &audiencepb.DashboardResponse{Leaderboard: leaderboard}
		return proto.Marshal(r)
	})
	if err != nil {
		return err
	}
	return e.Blob(http.StatusOK, "application/vnd.google.protobuf", res.([]byte))
}

var pushSubCacheLock sync.RWMutex
var pushSubCache map[string]int

func (*ContestantService) ListNotifications(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}

	contestant, _ := getCurrentContestant(e, db, false)

	var c int
	pushSubCacheLock.RLock()
	if cc, ok := pushSubCache[contestant.ID]; ok {
		c = cc
		pushSubCacheLock.RUnlock()
	} else {
		pushSubCacheLock.RUnlock()
		err := db.Get(
			&c,
			"SELECT count(*) FROM `push_subscriptions` WHERE `contestant_id` = ?",
			contestant.ID,
		)
		if err != nil {
			return fmt.Errorf("select push subscriptions: %w", err)
		}
		pushSubCacheLock.Lock()
		pushSubCache[contestant.ID] = c
		pushSubCacheLock.Unlock()
	}

	if c > 0 {
		return writeProto(e, http.StatusOK, &contestantpb.ListNotificationsResponse{
			Notifications: []*resourcespb.Notification{},
			// LastAnsweredClarificationId: lastAnsweredClarificationID,
		})
	}

	afterStr := e.QueryParam("after")

	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var notifications []*xsuportal.Notification
	if afterStr != "" {
		after, err := strconv.Atoi(afterStr)
		if err != nil {
			return fmt.Errorf("parse after: %w", err)
		}
		err = tx.Select(
			&notifications,
			"SELECT * FROM `notifications` WHERE `contestant_id` = ? AND `id` > ? ORDER BY `id`",
			contestant.ID,
			after,
		)
		if err != sql.ErrNoRows && err != nil {
			return fmt.Errorf("select notifications(after=%v): %w", after, err)
		}
	} else {
		err := tx.Select(
			&notifications,
			"SELECT * FROM `notifications` WHERE `contestant_id` = ? ORDER BY `id`",
			contestant.ID,
		)
		if err != sql.ErrNoRows && err != nil {
			return fmt.Errorf("select notifications: %w", err)
		}
	}

	if len(notifications) > 0 {
		nIDs := make([]string, 0)
		for _, n := range notifications {
			if !n.Read {
				nIDs = append(nIDs, strconv.FormatInt(n.ID, 10))
			}
		}
		if len(nIDs) > 0 {
			query := strings.Join(nIDs, ",")
			_, err := tx.Exec(
				"UPDATE `notifications` SET `read` = TRUE WHERE `contestant_id` = ? AND `read` = FALSE AND id IN ("+query+")",
				contestant.ID,
			)
			if err != nil {
				return fmt.Errorf("update notifications: %w", err)
			}
		}
	}
	team, _ := getCurrentTeam(e, tx, false)

	var lastAnsweredClarificationID int64
	err = tx.Get(
		&lastAnsweredClarificationID,
		"(SELECT `id` FROM `clarifications` WHERE `team_id` = ? AND `answered_at` IS NOT NULL ORDER BY `id` DESC LIMIT 1) UNION (SELECT `id` FROM `clarifications` WHERE `disclosed` = TRUE AND `answered_at` IS NOT NULL ORDER BY `id` DESC LIMIT 1) ORDER BY `id` DESC LIMIT 1",
		team.ID,
	)
	if err != sql.ErrNoRows && err != nil {
		return fmt.Errorf("get last answered clarification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	ns, err := makeNotificationsPB(notifications)
	if err != nil {
		return fmt.Errorf("make notifications: %w", err)
	}
	return writeProto(e, http.StatusOK, &contestantpb.ListNotificationsResponse{
		Notifications:               ns,
		LastAnsweredClarificationId: lastAnsweredClarificationID,
	})

}

func (*ContestantService) SubscribeNotification(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}

	if xsuportal.VAPIDKey() == nil {
		return halt(e, http.StatusServiceUnavailable, "WebPush は未対応です", nil)
	}

	var req contestantpb.SubscribeNotificationRequest
	if err := e.Bind(&req); err != nil {
		return err
	}

	contestant, _ := getCurrentContestant(e, db, false)
	_, err := db.Exec(
		"INSERT INTO `push_subscriptions` (`contestant_id`, `endpoint`, `p256dh`, `auth`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, NOW(6), NOW(6))",
		contestant.ID,
		req.Endpoint,
		req.P256Dh,
		req.Auth,
	)
	if err != nil {
		return fmt.Errorf("insert push_subscription: %w", err)
	}

	pushSubCacheLock.Lock()
	pushSubCache[contestant.ID] = 1
	pushSubCacheLock.Unlock()

	return writeProto(e, http.StatusOK, &contestantpb.SubscribeNotificationResponse{})
}

func (*ContestantService) UnsubscribeNotification(e echo.Context) error {
	if ok, err := loginRequired(e, db, &loginRequiredOption{Team: true}); !ok {
		return wrapError("check session", err)
	}

	if xsuportal.VAPIDKey() == nil {
		return halt(e, http.StatusServiceUnavailable, "WebPush は未対応です", nil)
	}

	var req contestantpb.UnsubscribeNotificationRequest
	if err := e.Bind(&req); err != nil {
		return err
	}

	contestant, _ := getCurrentContestant(e, db, false)
	_, err := db.Exec(
		"DELETE FROM `push_subscriptions` WHERE `contestant_id` = ? AND `endpoint` = ? LIMIT 1",
		contestant.ID,
		req.Endpoint,
	)
	if err != nil {
		return fmt.Errorf("delete push_subscription: %w", err)
	}

	pushSubCacheLock.Lock()
	pushSubCache[contestant.ID] = 0
	pushSubCacheLock.Unlock()

	return writeProto(e, http.StatusOK, &contestantpb.UnsubscribeNotificationResponse{})
}

func (*ContestantService) Signup(e echo.Context) error {
	var req contestantpb.SignupRequest
	if err := e.Bind(&req); err != nil {
		return err
	}

	hash := sha256.Sum256([]byte(req.Password))
	_, err := db.Exec(
		"INSERT INTO `contestants` (`id`, `password`, `staff`, `created_at`) VALUES (?, ?, FALSE, NOW(6))",
		req.ContestantId,
		hex.EncodeToString(hash[:]),
	)
	if mErr, ok := err.(*mysql.MySQLError); ok && mErr.Number == MYSQL_ER_DUP_ENTRY {
		return halt(e, http.StatusBadRequest, "IDが既に登録されています", nil)
	}
	if err != nil {
		return fmt.Errorf("insert contestant: %w", err)
	}
	sess, err := session.Get(SessionName, e)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	sess.Options = &sessions.Options{
		Path:   "/",
		MaxAge: 3600,
	}
	sess.Values["contestant_id"] = req.ContestantId
	if err := sess.Save(e.Request(), e.Response()); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return writeProto(e, http.StatusOK, &contestantpb.SignupResponse{})
}

func (*ContestantService) Login(e echo.Context) error {
	var req contestantpb.LoginRequest
	if err := e.Bind(&req); err != nil {
		return err
	}
	var password string
	err := db.Get(
		&password,
		"SELECT `password` FROM `contestants` WHERE `id` = ?",
		req.ContestantId,
	)
	if err != sql.ErrNoRows && err != nil {
		return fmt.Errorf("get contestant: %w", err)
	}
	passwordHash := sha256.Sum256([]byte(req.Password))
	digest := hex.EncodeToString(passwordHash[:])
	if err != sql.ErrNoRows && subtle.ConstantTimeCompare([]byte(digest), []byte(password)) == 1 {
		sess, err := session.Get(SessionName, e)
		if err != nil {
			return fmt.Errorf("get session: %w", err)
		}
		sess.Options = &sessions.Options{
			Path:   "/",
			MaxAge: 3600,
		}
		sess.Values["contestant_id"] = req.ContestantId
		if err := sess.Save(e.Request(), e.Response()); err != nil {
			return fmt.Errorf("save session: %w", err)
		}
	} else {
		return halt(e, http.StatusBadRequest, "ログインIDまたはパスワードが正しくありません", nil)
	}
	return writeProto(e, http.StatusOK, &contestantpb.LoginResponse{})
}

func (*ContestantService) Logout(e echo.Context) error {
	sess, err := session.Get(SessionName, e)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if _, ok := sess.Values["contestant_id"]; ok {
		delete(sess.Values, "contestant_id")
		sess.Options = &sessions.Options{
			Path:   "/",
			MaxAge: -1,
		}
		if err := sess.Save(e.Request(), e.Response()); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
	} else {
		return halt(e, http.StatusUnauthorized, "ログインしていません", nil)
	}
	return writeProto(e, http.StatusOK, &contestantpb.LogoutResponse{})
}

type RegistrationService struct{}

func (*RegistrationService) GetRegistrationSession(e echo.Context) error {
	var team *xsuportal.Team
	currentTeam, err := getCurrentTeam(e, db, false)
	if err != nil {
		return fmt.Errorf("get current team: %w", err)
	}
	team = currentTeam
	if team == nil {
		teamIDStr := e.QueryParam("team_id")
		inviteToken := e.QueryParam("invite_token")
		if teamIDStr != "" && inviteToken != "" {
			teamID, err := strconv.Atoi(teamIDStr)
			if err != nil {
				return fmt.Errorf("parse team id: %w", err)
			}
			var t xsuportal.Team
			err = db.Get(
				&t,
				"SELECT * FROM `teams` WHERE `id` = ? AND `invite_token` = ? AND `withdrawn` = FALSE",
				teamID,
				inviteToken,
			)
			if err == sql.ErrNoRows {
				return halt(e, http.StatusNotFound, "招待URLが無効です", nil)
			}
			if err != nil {
				return fmt.Errorf("get team: %w", err)
			}
			team = &t
		}
	}

	var members []xsuportal.Contestant
	if team != nil {
		err := db.Select(
			&members,
			"SELECT * FROM `contestants` WHERE `team_id` = ?",
			team.ID,
		)
		if err != nil {
			return fmt.Errorf("select members: %w", err)
		}
	}

	res := &registrationpb.GetRegistrationSessionResponse{
		Status: 0,
	}
	contestant, err := getCurrentContestant(e, db, false)
	if err != nil {
		return fmt.Errorf("get current contestant: %w", err)
	}
	switch {
	case contestant != nil && contestant.TeamID.Valid:
		res.Status = registrationpb.GetRegistrationSessionResponse_JOINED
	case team != nil && len(members) >= 3:
		res.Status = registrationpb.GetRegistrationSessionResponse_NOT_JOINABLE
	case contestant == nil:
		res.Status = registrationpb.GetRegistrationSessionResponse_NOT_LOGGED_IN
	case team != nil:
		res.Status = registrationpb.GetRegistrationSessionResponse_JOINABLE
	case team == nil:
		res.Status = registrationpb.GetRegistrationSessionResponse_CREATABLE
	default:
		return fmt.Errorf("undeterminable status")
	}
	if team != nil {
		res.Team, err = makeTeamPB(db, team, contestant != nil && currentTeam != nil && contestant.ID == currentTeam.LeaderID.String, true)
		if err != nil {
			return fmt.Errorf("make team: %w", err)
		}
		res.MemberInviteUrl = fmt.Sprintf("/registration?team_id=%v&invite_token=%v", team.ID, team.InviteToken)
		res.InviteToken = team.InviteToken
	}
	return writeProto(e, http.StatusOK, res)
}

func (*RegistrationService) CreateTeam(e echo.Context) error {
	var req registrationpb.CreateTeamRequest
	if err := e.Bind(&req); err != nil {
		return err
	}
	if ok, err := loginRequired(e, db, &loginRequiredOption{}); !ok {
		return wrapError("check session", err)
	}
	ok, err := contestStatusRestricted(e, db, resourcespb.Contest_REGISTRATION, "チーム登録期間ではありません")
	if !ok {
		return wrapError("check contest status", err)
	}

	ctx := context.Background()
	conn, err := db.Connx(ctx)
	if err != nil {
		return fmt.Errorf("get conn: %w", err)
	}
	defer conn.Close()

	_, err = conn.ExecContext(ctx, "LOCK TABLES `teams` WRITE, `contestants` WRITE")
	if err != nil {
		return fmt.Errorf("lock tables: %w", err)
	}
	defer conn.ExecContext(ctx, "UNLOCK TABLES")

	randomBytes := make([]byte, 64)
	_, err = rand.Read(randomBytes)
	if err != nil {
		return fmt.Errorf("read random: %w", err)
	}
	inviteToken := base64.URLEncoding.EncodeToString(randomBytes)
	var withinCapacity bool
	err = conn.QueryRowContext(
		ctx,
		"SELECT COUNT(*) < ? AS `within_capacity` FROM `teams`",
		TeamCapacity,
	).Scan(&withinCapacity)
	if err != nil {
		return fmt.Errorf("check capacity: %w", err)
	}
	if !withinCapacity {
		return halt(e, http.StatusForbidden, "チーム登録数上限です", nil)
	}
	_, err = conn.ExecContext(
		ctx,
		"INSERT INTO `teams` (`name`, `email_address`, `invite_token`, `created_at`) VALUES (?, ?, ?, NOW(6))",
		req.TeamName,
		req.EmailAddress,
		inviteToken,
	)
	if err != nil {
		return fmt.Errorf("insert team: %w", err)
	}
	var teamID int64
	err = conn.QueryRowContext(
		ctx,
		"SELECT LAST_INSERT_ID() AS `id`",
	).Scan(&teamID)
	if err != nil || teamID == 0 {
		return halt(e, http.StatusInternalServerError, "チームを登録できませんでした", nil)
	}

	_, err = conn.ExecContext(
		ctx,
		"INSERT INTO `team_scores` (`id`) VALUES (?)",
		teamID,
	)
	if err != nil {
		return fmt.Errorf("insert team_scores: %w", err)
	}

	contestant, _ := getCurrentContestant(e, db, false)

	_, err = conn.ExecContext(
		ctx,
		"UPDATE `contestants` SET `name` = ?, `student` = ?, `team_id` = ? WHERE id = ?",
		req.Name,
		req.IsStudent,
		teamID,
		contestant.ID,
	)
	if err != nil {
		return fmt.Errorf("update contestant: %w", err)
	}

	_, err = conn.ExecContext(
		ctx,
		"UPDATE `teams` SET `leader_id` = ? WHERE `id` = ?",
		contestant.ID,
		teamID,
	)
	if err != nil {
		return fmt.Errorf("update team: %w", err)
	}

	_, err = conn.ExecContext(
		ctx,
		"UPDATE teams LEFT JOIN (SELECT team_id,(SUM(`student`) = COUNT(*)) AS `student` FROM `contestants` WHERE `contestants`.`team_id` = ?) `team_student_flags` ON `teams`.`id` = `team_student_flags`.`team_id` SET `teams`.`student_flag` = `team_student_flags`.`student` WHERE `teams`.`id` = ?",
		teamID,
		teamID,
	)
	if err != nil {
		return fmt.Errorf("update team: %w", err)
	}

	return writeProto(e, http.StatusOK, &registrationpb.CreateTeamResponse{
		TeamId: teamID,
	})
}

func (*RegistrationService) JoinTeam(e echo.Context) error {
	var req registrationpb.JoinTeamRequest
	if err := e.Bind(&req); err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if ok, err := loginRequired(e, tx, &loginRequiredOption{Lock: true}); !ok {
		return wrapError("check session", err)
	}
	if ok, err := contestStatusRestricted(e, tx, resourcespb.Contest_REGISTRATION, "チーム登録期間ではありません"); !ok {
		return wrapError("check contest status", err)
	}
	var team xsuportal.Team
	err = tx.Get(
		&team,
		"SELECT * FROM `teams` WHERE `id` = ? AND `invite_token` = ? AND `withdrawn` = FALSE FOR UPDATE",
		req.TeamId,
		req.InviteToken,
	)
	if err == sql.ErrNoRows {
		return halt(e, http.StatusBadRequest, "招待URLが不正です", nil)
	}
	if err != nil {
		return fmt.Errorf("get team with lock: %w", err)
	}
	var memberCount int
	err = tx.Get(
		&memberCount,
		"SELECT COUNT(*) AS `cnt` FROM `contestants` WHERE `team_id` = ?",
		req.TeamId,
	)
	if err != nil {
		return fmt.Errorf("count team member: %w", err)
	}
	if memberCount >= 3 {
		return halt(e, http.StatusBadRequest, "チーム人数の上限に達しています", nil)
	}

	contestant, _ := getCurrentContestant(e, tx, false)
	_, err = tx.Exec(
		"UPDATE `contestants` SET `team_id` = ?, `name` = ?, `student` = ? WHERE `id` = ?",
		req.TeamId,
		req.Name,
		req.IsStudent,
		contestant.ID,
	)
	if err != nil {
		return fmt.Errorf("update contestant: %w", err)
	}

	_, err = tx.Exec(
		"UPDATE teams LEFT JOIN (SELECT team_id,(SUM(`student`) = COUNT(*)) AS `student` FROM `contestants` WHERE `contestants`.`team_id` = ?) `team_student_flags` ON `teams`.`id` = `team_student_flags`.`team_id` SET `teams`.`student_flag` = `team_student_flags`.`student` WHERE `teams`.`id` = ?",
		req.TeamId,
		req.TeamId,
	)
	if err != nil {
		return fmt.Errorf("update team: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return writeProto(e, http.StatusOK, &registrationpb.JoinTeamResponse{})
}

func (*RegistrationService) UpdateRegistration(e echo.Context) error {
	var req registrationpb.UpdateRegistrationRequest
	if err := e.Bind(&req); err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if ok, err := loginRequired(e, tx, &loginRequiredOption{Team: true, Lock: true}); !ok {
		return wrapError("check session", err)
	}
	team, _ := getCurrentTeam(e, tx, false)
	contestant, _ := getCurrentContestant(e, tx, false)
	if team.LeaderID.Valid && team.LeaderID.String == contestant.ID {
		_, err := tx.Exec(
			"UPDATE `teams` SET `name` = ?, `email_address` = ? WHERE `id` = ?",
			req.TeamName,
			req.EmailAddress,
			team.ID,
		)
		if err != nil {
			return fmt.Errorf("update team: %w", err)
		}
	}
	_, err = tx.Exec(
		"UPDATE `contestants` SET `name` = ?, `student` = ? WHERE `id` = ?",
		req.Name,
		req.IsStudent,
		contestant.ID,
	)
	if err != nil {
		return fmt.Errorf("update contestant: %w", err)
	}

	_, err = tx.Exec(
		"UPDATE teams LEFT JOIN (SELECT team_id,(SUM(`student`) = COUNT(*)) AS `student` FROM `contestants` WHERE `contestants`.`team_id` = ?) `team_student_flags` ON `teams`.`id` = `team_student_flags`.`team_id` SET `teams`.`student_flag` = `team_student_flags`.`student` WHERE `teams`.`id` = ?",
		team.ID,
		team.ID,
	)
	if err != nil {
		return fmt.Errorf("update team: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return writeProto(e, http.StatusOK, &registrationpb.UpdateRegistrationResponse{})
}

func (*RegistrationService) DeleteRegistration(e echo.Context) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if ok, err := loginRequired(e, tx, &loginRequiredOption{Team: true, Lock: true}); !ok {
		return wrapError("check session", err)
	}
	if ok, err := contestStatusRestricted(e, tx, resourcespb.Contest_REGISTRATION, "チーム登録期間外は辞退できません"); !ok {
		return wrapError("check contest status", err)
	}
	team, _ := getCurrentTeam(e, tx, false)
	contestant, _ := getCurrentContestant(e, tx, false)
	if team.LeaderID.Valid && team.LeaderID.String == contestant.ID {
		_, err := tx.Exec(
			"UPDATE `teams` SET `withdrawn` = TRUE, `leader_id` = NULL WHERE `id` = ?",
			team.ID,
		)
		if err != nil {
			return fmt.Errorf("withdrawn team(id=%v): %w", team.ID, err)
		}
		_, err = tx.Exec(
			"UPDATE `contestants` SET `team_id` = NULL WHERE `team_id` = ?",
			team.ID,
		)
		if err != nil {
			return fmt.Errorf("withdrawn members(team_id=%v): %w", team.ID, err)
		}
	} else {
		_, err := tx.Exec(
			"UPDATE `contestants` SET `team_id` = NULL WHERE `id` = ?",
			contestant.ID,
		)
		if err != nil {
			return fmt.Errorf("withdrawn contestant(id=%v): %w", contestant.ID, err)
		}
	}

	_, err = tx.Exec(
		"UPDATE teams LEFT JOIN (SELECT team_id,(SUM(`student`) = COUNT(*)) AS `student` FROM `contestants` GROUP BY `contestants`.`team_id`) `team_student_flags` ON `teams`.`id` = `team_student_flags`.`team_id` SET `teams`.`student` = `team_student_flags`.`student`",
	)
	if err != nil {
		return fmt.Errorf("update team: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return writeProto(e, http.StatusOK, &registrationpb.DeleteRegistrationResponse{})
}

type AudienceService struct{}

func (*AudienceService) ListTeams(e echo.Context) error {
	var teams []xsuportal.Team
	err := db.Select(&teams, "SELECT * FROM `teams` WHERE `withdrawn` = FALSE ORDER BY `created_at` DESC")
	if err != nil {
		return fmt.Errorf("select teams: %w", err)
	}
	res := &audiencepb.ListTeamsResponse{}
	for _, team := range teams {
		var members []xsuportal.Contestant
		err := db.Select(
			&members,
			"SELECT * FROM `contestants` WHERE `team_id` = ? ORDER BY `created_at`",
			team.ID,
		)
		if err != nil {
			return fmt.Errorf("select members(team_id=%v): %w", team.ID, err)
		}
		var memberNames []string
		isStudent := true
		for _, member := range members {
			memberNames = append(memberNames, member.Name.String)
			isStudent = isStudent && member.Student
		}
		res.Teams = append(res.Teams, &audiencepb.ListTeamsResponse_TeamListItem{
			TeamId:      team.ID,
			Name:        team.Name,
			MemberNames: memberNames,
			IsStudent:   isStudent,
		})
	}
	return writeProto(e, http.StatusOK, res)
}

var audienceDashboardCacheLock sync.RWMutex
var audienceDashboardCacheTime int64
var audienceDashboardCache []byte

func backgroundLeaderboardPB() {
	for {
		start := time.Now()
		leaderboard, err := makeLeaderboardPB(0)
		end := time.Now()
		log.Printf("%f秒\n", (end.Sub(start)).Seconds())
		n := time.Now()
		if err == nil {
			r := &audiencepb.DashboardResponse{Leaderboard: leaderboard}
			res, _ := proto.Marshal(r)
			var buffer bytes.Buffer
			ww := gzip.NewWriter(&buffer)
			ww.Write(res)
			ww.Close()
			res = buffer.Bytes()
			audienceDashboardCacheLock.Lock()
			audienceDashboardCache = res
			cs, _ := getCurrentContestStatus(db)
			if cs.ContestFreezesAt.Unix() > n.Unix() {
				audienceDashboardCacheTime = n.UnixNano() + 700000
			} else {
				audienceDashboardCacheTime = n.UnixNano() + 700000
			}
			audienceDashboardCacheLock.Unlock()
		} else {
			log.Printf("makeLeaderboardPB %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (*AudienceService) Dashboard(e echo.Context) error {
	audienceDashboardCacheLock.RLock()
	if audienceDashboardCache != nil && audienceDashboardCacheTime > time.Now().UnixNano() {
		defer audienceDashboardCacheLock.RUnlock()
		e.Response().Header().Set("Content-Encoding", "gzip")
		return e.Blob(http.StatusOK, "application/vnd.google.protobuf", audienceDashboardCache)
	}
	audienceDashboardCacheLock.RUnlock()
	res, err, _ := sfGroup.Do("dashboard", func() (interface{}, error) {
		leaderboard, err := makeLeaderboardPB(0)
		if err != nil {
			return nil, fmt.Errorf("make leaderboard: %w", err)
		}
		r := &audiencepb.DashboardResponse{Leaderboard: leaderboard}
		return proto.Marshal(r)
	})
	if err != nil {
		return err
	}
	return e.Blob(http.StatusOK, "application/vnd.google.protobuf", res.([]byte))
}

type XsuportalContext struct {
	Contestant *xsuportal.Contestant
	Team       *xsuportal.Team
}

func getXsuportalContext(e echo.Context) *XsuportalContext {
	xc := e.Get("xsucon_context")
	if xc == nil {
		xc = &XsuportalContext{}
		e.Set("xsucon_context", xc)
	}
	return xc.(*XsuportalContext)
}

func getCurrentContestant(e echo.Context, db sqlx.Queryer, lock bool) (*xsuportal.Contestant, error) {
	xc := getXsuportalContext(e)
	if xc.Contestant != nil {
		return xc.Contestant, nil
	}
	sess, err := session.Get(SessionName, e)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	contestantID, ok := sess.Values["contestant_id"]
	if !ok {
		return nil, nil
	}
	var contestant xsuportal.Contestant
	query := "SELECT * FROM `contestants` WHERE `id` = ?"
	if lock {
		query += " FOR UPDATE"
	}
	err = sqlx.Get(db, &contestant, query, contestantID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query contestant: %w", err)
	}
	xc.Contestant = &contestant
	return xc.Contestant, nil
}

func getCurrentTeam(e echo.Context, db sqlx.Queryer, lock bool) (*xsuportal.Team, error) {
	xc := getXsuportalContext(e)
	if xc.Team != nil {
		return xc.Team, nil
	}
	contestant, err := getCurrentContestant(e, db, false)
	if err != nil {
		return nil, fmt.Errorf("current contestant: %w", err)
	}
	if contestant == nil {
		return nil, nil
	}
	var team xsuportal.Team
	query := "SELECT * FROM `teams` WHERE `id` = ?"
	if lock {
		query += " FOR UPDATE"
	}
	err = sqlx.Get(db, &team, query, contestant.TeamID.Int64)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query team: %w", err)
	}
	xc.Team = &team
	return xc.Team, nil
}

var contestStatusCacheLock sync.RWMutex
var contestStatusCacheTime int64
var contestStatusCache *xsuportal.ContestStatus

func getCurrentContestStatus(db sqlx.Queryer) (*xsuportal.ContestStatus, error) {
	var contestStatus xsuportal.ContestStatus

	contestStatusCacheLock.RLock()
	if contestStatusCache != nil && contestStatusCacheTime > time.Now().Unix() {
		// cache
		contestStatus.RegistrationOpenAt = contestStatusCache.RegistrationOpenAt
		contestStatus.ContestStartsAt = contestStatusCache.ContestStartsAt
		contestStatus.ContestFreezesAt = contestStatusCache.ContestFreezesAt
		contestStatus.ContestEndsAt = contestStatusCache.ContestEndsAt
		contestStatusCacheLock.RUnlock()
	} else {
		contestStatusCacheLock.RUnlock()
		err := sqlx.Get(db, &contestStatus, "SELECT *, NOW(6) AS `current_time`, CASE WHEN NOW(6) < `registration_open_at` THEN 'standby' WHEN `registration_open_at` <= NOW(6) AND NOW(6) < `contest_starts_at` THEN 'registration' WHEN `contest_starts_at` <= NOW(6) AND NOW(6) < `contest_ends_at` THEN 'started' WHEN `contest_ends_at` <= NOW(6) THEN 'finished' ELSE 'unknown' END AS `status`, IF(`contest_starts_at` <= NOW(6) AND NOW(6) < `contest_freezes_at`, 1, 0) AS `frozen` FROM `contest_config`")
		if err != nil {
			return nil, fmt.Errorf("query contest status: %w", err)
		}
		contestStatusCacheLock.Lock()
		contestStatusCacheTime = time.Now().Unix() + 30
		contestStatusCache = &xsuportal.ContestStatus{}
		contestStatusCache.RegistrationOpenAt = contestStatus.RegistrationOpenAt
		contestStatusCache.ContestStartsAt = contestStatus.ContestStartsAt
		contestStatusCache.ContestFreezesAt = contestStatus.ContestFreezesAt
		contestStatusCache.ContestEndsAt = contestStatus.ContestEndsAt
		contestStatusCacheLock.Unlock()
	}

	now := time.Now()
	unixnow := now.Unix()
	contestStatus.CurrentTime = now

	/*
	   CASE
	   WHEN NOW(6) < `registration_open_at` THEN 'standby'
	   WHEN `registration_open_at` <= NOW(6) AND NOW(6) < `contest_starts_at` THEN 'registration'
	   WHEN `contest_starts_at` <= NOW(6) AND NOW(6) < `contest_ends_at` THEN 'started'
	   WHEN `contest_ends_at` <= NOW(6) THEN 'finished'
	   ELSE 'unknown' END AS `status`,
	*/

	contestStatus.CurrentTime = now
	if unixnow < contestStatus.RegistrationOpenAt.Unix() {
		contestStatus.StatusStr = "standby"
	} else if contestStatus.RegistrationOpenAt.Unix() <= unixnow && unixnow < contestStatus.ContestStartsAt.Unix() {
		contestStatus.StatusStr = "registration"
	} else if contestStatus.ContestStartsAt.Unix() <= unixnow && unixnow < contestStatus.ContestEndsAt.Unix() {
		contestStatus.StatusStr = "started"
	} else if contestStatus.ContestEndsAt.Unix() <= unixnow {
		contestStatus.StatusStr = "finished"
	} else {
		contestStatus.StatusStr = "status"
	}
	/*
	   IF(`contest_starts_at` <= NOW(6) AND NOW(6) < `contest_freezes_at`, 1, 0) AS `frozen`
	*/
	if contestStatus.ContestStartsAt.Unix() <= unixnow && unixnow < contestStatus.ContestFreezesAt.Unix() {
		contestStatus.Frozen = true
	} else {
		contestStatus.Frozen = false
	}

	statusStr := contestStatus.StatusStr
	/*if e.Echo().Debug {
		b, err := ioutil.ReadFile(DebugContestStatusFilePath)
		if err == nil {
			statusStr = string(b)
		}
	}*/
	switch statusStr {
	case "standby":
		contestStatus.Status = resourcespb.Contest_STANDBY
	case "registration":
		contestStatus.Status = resourcespb.Contest_REGISTRATION
	case "started":
		contestStatus.Status = resourcespb.Contest_STARTED
	case "finished":
		contestStatus.Status = resourcespb.Contest_FINISHED
	default:
		return nil, fmt.Errorf("unexpected contest status: %q", contestStatus.StatusStr)
	}
	return &contestStatus, nil
}

type loginRequiredOption struct {
	Team bool
	Lock bool
}

func loginRequired(e echo.Context, db sqlx.Queryer, option *loginRequiredOption) (bool, error) {
	contestant, err := getCurrentContestant(e, db, option.Lock)
	if err != nil {
		return false, fmt.Errorf("current contestant: %w", err)
	}
	if contestant == nil {
		return false, halt(e, http.StatusUnauthorized, "ログインが必要です", nil)
	}
	if option.Team {
		t, err := getCurrentTeam(e, db, option.Lock)
		if err != nil {
			return false, fmt.Errorf("current team: %w", err)
		}
		if t == nil {
			return false, halt(e, http.StatusForbidden, "参加登録が必要です", nil)
		}
	}
	return true, nil
}

func contestStatusRestricted(e echo.Context, db sqlx.Queryer, status resourcespb.Contest_Status, message string) (bool, error) {
	contestStatus, err := getCurrentContestStatus(db)
	if err != nil {
		return false, fmt.Errorf("get current contest status: %w", err)
	}
	if contestStatus.Status != status {
		return false, halt(e, http.StatusForbidden, message, nil)
	}
	return true, nil
}

func writeProto(e echo.Context, code int, m proto.Message) error {
	res, _ := proto.Marshal(m)
	return e.Blob(code, "application/vnd.google.protobuf", res)
}

func halt(e echo.Context, code int, humanMessage string, err error) error {
	message := &xsuportalpb.Error{
		Code: int32(code),
	}
	if err != nil {
		message.Name = fmt.Sprintf("%T", err)
		message.HumanMessage = err.Error()
		message.HumanDescriptions = strings.Split(fmt.Sprintf("%+v", err), "\n")
	}
	if humanMessage != "" {
		message.HumanMessage = humanMessage
		message.HumanDescriptions = []string{humanMessage}
	}
	res, _ := proto.Marshal(message)
	return e.Blob(code, "application/vnd.google.protobuf; proto=xsuportal.proto.Error", res)
}

func makeClarificationPB(db sqlx.Queryer, c *xsuportal.Clarification, t *xsuportal.Team) (*resourcespb.Clarification, error) {
	team, err := makeTeamPB(db, t, false, true)
	if err != nil {
		return nil, fmt.Errorf("make team: %w", err)
	}
	pb := &resourcespb.Clarification{
		Id:        c.ID,
		TeamId:    c.TeamID,
		Answered:  c.AnsweredAt.Valid,
		Disclosed: c.Disclosed.Bool,
		Question:  c.Question.String,
		Answer:    c.Answer.String,
		CreatedAt: timestamppb.New(c.CreatedAt),
		Team:      team,
	}
	if c.AnsweredAt.Valid {
		pb.AnsweredAt = timestamppb.New(c.AnsweredAt.Time)
	}
	return pb, nil
}

func makeTeamPB(db sqlx.Queryer, t *xsuportal.Team, detail bool, enableMembers bool) (*resourcespb.Team, error) {
	pb := &resourcespb.Team{
		Id:        t.ID,
		Name:      t.Name,
		LeaderId:  t.LeaderID.String,
		Withdrawn: t.Withdrawn,
	}
	if detail {
		pb.Detail = &resourcespb.Team_TeamDetail{
			EmailAddress: t.EmailAddress,
			InviteToken:  t.InviteToken,
		}
	}
	if enableMembers {
		if t.LeaderID.Valid {
			var leader xsuportal.Contestant
			if err := sqlx.Get(db, &leader, "SELECT * FROM `contestants` WHERE `id` = ?", t.LeaderID.String); err != nil {
				return nil, fmt.Errorf("get leader: %w", err)
			}
			pb.Leader = makeContestantPB(&leader)
		}
		var members []xsuportal.Contestant
		if err := sqlx.Select(db, &members, "SELECT * FROM `contestants` WHERE `team_id` = ? ORDER BY `created_at`", t.ID); err != nil {
			return nil, fmt.Errorf("select members: %w", err)
		}
		for _, member := range members {
			pb.Members = append(pb.Members, makeContestantPB(&member))
			pb.MemberIds = append(pb.MemberIds, member.ID)
		}
	}
	if t.Student.Valid {
		pb.Student = &resourcespb.Team_StudentStatus{
			Status: t.Student.Bool,
		}
	}
	return pb, nil
}

func makeContestantPB(c *xsuportal.Contestant) *resourcespb.Contestant {
	return &resourcespb.Contestant{
		Id:        c.ID,
		TeamId:    c.TeamID.Int64,
		Name:      c.Name.String,
		IsStudent: c.Student,
		IsStaff:   c.Staff,
	}
}

func makeContestPB(e echo.Context) (*resourcespb.Contest, error) {
	contestStatus, err := getCurrentContestStatus(db)
	if err != nil {
		return nil, fmt.Errorf("get current contest status: %w", err)
	}
	return &resourcespb.Contest{
		RegistrationOpenAt: timestamppb.New(contestStatus.RegistrationOpenAt),
		ContestStartsAt:    timestamppb.New(contestStatus.ContestStartsAt),
		ContestFreezesAt:   timestamppb.New(contestStatus.ContestFreezesAt),
		ContestEndsAt:      timestamppb.New(contestStatus.ContestEndsAt),
		Status:             contestStatus.Status,
		Frozen:             contestStatus.Frozen,
	}, nil
}

func makeLeaderboardPB(teamID int64) (*resourcespb.Leaderboard, error) {
	contestStatus, err := getCurrentContestStatus(db)
	if err != nil {
		return nil, fmt.Errorf("get current contest status: %w", err)
	}
	contestFinished := contestStatus.Status == resourcespb.Contest_FINISHED
	contestFreezesAt := contestStatus.ContestFreezesAt

	tx, err := db.BeginTxx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	var leaderboard []xsuportal.LeaderBoardTeam
	if contestFreezesAt.Before(time.Now()) && !contestFinished {
		query := "SELECT\n" +
			"  `teams`.`id` AS `id`,\n" +
			"  `teams`.`name` AS `name`,\n" +
			"  `teams`.`leader_id` AS `leader_id`,\n" +
			"  `teams`.`withdrawn` AS `withdrawn`,\n" +
			"  `teams`.`student_flag` AS `student`,\n" +
			"  (`best_score_jobs`.`score_raw` - `best_score_jobs`.`score_deduction`) AS `best_score`,\n" +
			"  `best_score_jobs`.`started_at` AS `best_score_started_at`,\n" +
			"  `best_score_jobs`.`finished_at` AS `best_score_marked_at`,\n" +
			"  (`latest_score_jobs`.`score_raw` - `latest_score_jobs`.`score_deduction`) AS `latest_score`,\n" +
			"  `latest_score_jobs`.`started_at` AS `latest_score_started_at`,\n" +
			"  `latest_score_jobs`.`finished_at` AS `latest_score_marked_at`,\n" +
			"  `latest_score_job_ids`.`finish_count` AS `finish_count`\n" +
			"FROM\n" +
			"  `teams`\n" +
			"  -- latest scores\n" +
			"  LEFT JOIN (\n" +
			"    SELECT\n" +
			"      MAX(`id`) AS `id`,\n" +
			"      `team_id`,\n" +
			"      COUNT(*) AS `finish_count`\n" +
			"    FROM\n" +
			"      `benchmark_jobs`\n" +
			"    WHERE\n" +
			"      `finished_at` IS NOT NULL\n" +
			"      -- score freeze\n" +
			"      AND (`team_id` = ? OR (`team_id` != ? AND (? = TRUE OR `finished_at` < ?)))\n" +
			"    GROUP BY\n" +
			"      `team_id`\n" +
			"  ) `latest_score_job_ids` ON `latest_score_job_ids`.`team_id` = `teams`.`id`\n" +
			"  LEFT JOIN `benchmark_jobs` `latest_score_jobs` ON `latest_score_job_ids`.`id` = `latest_score_jobs`.`id`\n" +
			"  -- best scores\n" +
			"  LEFT JOIN (\n" +
			"    SELECT\n" +
			"      MAX(`j`.`id`) AS `id`,\n" +
			"      `j`.`team_id` AS `team_id`\n" +
			"    FROM\n" +
			"      (\n" +
			"        SELECT\n" +
			"          `team_id`,\n" +
			"          MAX(`score_raw` - `score_deduction`) AS `score`\n" +
			"        FROM\n" +
			"          `benchmark_jobs`\n" +
			"        WHERE\n" +
			"          `finished_at` IS NOT NULL\n" +
			"          -- score freeze\n" +
			"          AND (`team_id` = ? OR (`team_id` != ? AND (? = TRUE OR `finished_at` < ?)))\n" +
			"        GROUP BY\n" +
			"          `team_id`\n" +
			"      ) `best_scores`\n" +
			"      LEFT JOIN `benchmark_jobs` `j` ON (`j`.`score_raw` - `j`.`score_deduction`) = `best_scores`.`score`\n" +
			"        AND `j`.`team_id` = `best_scores`.`team_id`\n" +
			"    GROUP BY\n" +
			"      `j`.`team_id`\n" +
			"  ) `best_score_job_ids` ON `best_score_job_ids`.`team_id` = `teams`.`id`\n" +
			"  LEFT JOIN `benchmark_jobs` `best_score_jobs` ON `best_score_jobs`.`id` = `best_score_job_ids`.`id`\n" +
			"ORDER BY\n" +
			"  `latest_score` DESC,\n" +
			"  `latest_score_marked_at` ASC,\n" +
			"  `teams`.`id` ASC\n"
		err = tx.Select(&leaderboard, query, teamID, teamID, contestFinished, contestFreezesAt, teamID, teamID, contestFinished, contestFreezesAt)
		if err != sql.ErrNoRows && err != nil {
			return nil, fmt.Errorf("select leaderboard: %w", err)
		}
	} else {
		query := "SELECT " +
			"`teams`.`id` AS `id`, " +
			"`teams.`name` AS `name`, " +
			"`teams`.`leader_id` AS `leader_id`, " +
			"`teams`.`withdrawn` AS `withdrawn`, " +
			"`teams`.`student` AS `student`, " +
			"`ts`.`best_score` AS `best_score`, " +
			"`ts`.`best_started_at` AS `best_score_started_at`, " +
			"`ts`.`best_finished_at` AS `best_score_marked_at`, " +
			"`ts`.`latest_score` AS `latest_score`, " +
			"`ts`.`latest_started_at` AS `latest_score_started_at`, " +
			"`ts`.`latest_finished_at` AS `latest_score_marked_at`, " +
			"`ts`.`finish_count` AS `finish_count` " +
			"FROM `teams` LEFT JOIN `team_scores` `ts` ON `teams`.`id` = `ts`.`team_id` " +
			"ORDER BY `latest_score` DESC, `latest_score_marked_at` ASC, `teams`.`id` ASC"
		err = tx.Select(&leaderboard, query)
		if err != sql.ErrNoRows && err != nil {
			return nil, fmt.Errorf("select leaderboard: %w", err)
		}
	}
	jobResultsQuery := "SELECT\n" +
		"  `team_id` AS `team_id`,\n" +
		"  (`score_raw` - `score_deduction`) AS `score`,\n" +
		"  `started_at` AS `started_at`,\n" +
		"  `finished_at` AS `finished_at`\n" +
		"FROM\n" +
		"  `benchmark_jobs`\n" +
		"WHERE\n" +
		"    `finished_at` IS NOT NULL\n" +
		"    -- score freeze\n" +
		"    AND (`team_id` = ? OR (`team_id` != ? AND (? = TRUE OR `finished_at` < ?)))\n" +
		"ORDER BY\n" +
		"  `finished_at`"
	var jobResults []xsuportal.JobResult
	err = tx.Select(&jobResults, jobResultsQuery, teamID, teamID, contestFinished, contestFreezesAt)
	if err != sql.ErrNoRows && err != nil {
		return nil, fmt.Errorf("select job results: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	teamGraphScores := make(map[int64][]*resourcespb.Leaderboard_LeaderboardItem_LeaderboardScore)
	for _, jobResult := range jobResults {
		teamGraphScores[jobResult.TeamID] = append(teamGraphScores[jobResult.TeamID], &resourcespb.Leaderboard_LeaderboardItem_LeaderboardScore{
			Score:     jobResult.Score,
			StartedAt: timestamppb.New(jobResult.StartedAt),
			MarkedAt:  timestamppb.New(jobResult.FinishedAt),
		})
	}
	pb := &resourcespb.Leaderboard{}
	for _, team := range leaderboard {
		t, _ := makeTeamPB(db, team.Team(), false, false)
		item := &resourcespb.Leaderboard_LeaderboardItem{
			Scores: teamGraphScores[team.ID],
			BestScore: &resourcespb.Leaderboard_LeaderboardItem_LeaderboardScore{
				Score:     team.BestScore.Int64,
				StartedAt: toTimestamp(team.BestScoreStartedAt),
				MarkedAt:  toTimestamp(team.BestScoreMarkedAt),
			},
			LatestScore: &resourcespb.Leaderboard_LeaderboardItem_LeaderboardScore{
				Score:     team.LatestScore.Int64,
				StartedAt: toTimestamp(team.LatestScoreStartedAt),
				MarkedAt:  toTimestamp(team.LatestScoreMarkedAt),
			},
			Team:        t,
			FinishCount: team.FinishCount.Int64,
		}
		if team.Student.Valid && team.Student.Bool {
			pb.StudentTeams = append(pb.StudentTeams, item)
		} else {
			pb.GeneralTeams = append(pb.GeneralTeams, item)
		}
		pb.Teams = append(pb.Teams, item)
	}
	return pb, nil
}

func makeBenchmarkJobPB(job *xsuportal.BenchmarkJob) *resourcespb.BenchmarkJob {
	pb := &resourcespb.BenchmarkJob{
		Id:             job.ID,
		TeamId:         job.TeamID,
		Status:         resourcespb.BenchmarkJob_Status(job.Status),
		TargetHostname: job.TargetHostName,
		CreatedAt:      timestamppb.New(job.CreatedAt),
		UpdatedAt:      timestamppb.New(job.UpdatedAt),
	}
	if job.StartedAt.Valid {
		pb.StartedAt = timestamppb.New(job.StartedAt.Time)
	}
	if job.FinishedAt.Valid {
		pb.FinishedAt = timestamppb.New(job.FinishedAt.Time)
		pb.Result = makeBenchmarkResultPB(job)
	}
	return pb
}

func makeBenchmarkResultPB(job *xsuportal.BenchmarkJob) *resourcespb.BenchmarkResult {
	hasScore := job.ScoreRaw.Valid && job.ScoreDeduction.Valid
	pb := &resourcespb.BenchmarkResult{
		Finished: job.FinishedAt.Valid,
		Passed:   job.Passed.Bool,
		Reason:   job.Reason.String,
	}
	if hasScore {
		pb.Score = int64(job.ScoreRaw.Int32 - job.ScoreDeduction.Int32)
		pb.ScoreBreakdown = &resourcespb.BenchmarkResult_ScoreBreakdown{
			Raw:       int64(job.ScoreRaw.Int32),
			Deduction: int64(job.ScoreDeduction.Int32),
		}
	}
	return pb
}

func makeBenchmarkJobsPB(e echo.Context, db sqlx.Queryer, limit int) ([]*resourcespb.BenchmarkJob, error) {
	team, _ := getCurrentTeam(e, db, false)
	query := "SELECT * FROM `benchmark_jobs` WHERE `team_id` = ? ORDER BY `created_at` DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	var jobs []xsuportal.BenchmarkJob
	if err := sqlx.Select(db, &jobs, query, team.ID); err != nil {
		return nil, fmt.Errorf("select benchmark jobs: %w", err)
	}
	var benchmarkJobs []*resourcespb.BenchmarkJob
	for _, job := range jobs {
		benchmarkJobs = append(benchmarkJobs, makeBenchmarkJobPB(&job))
	}
	return benchmarkJobs, nil
}

func makeNotificationsPB(notifications []*xsuportal.Notification) ([]*resourcespb.Notification, error) {
	var ns []*resourcespb.Notification
	for _, notification := range notifications {
		decoded, err := base64.StdEncoding.DecodeString(notification.EncodedMessage)
		if err != nil {
			return nil, fmt.Errorf("decode message: %w", err)
		}
		var message resourcespb.Notification
		if err := proto.Unmarshal(decoded, &message); err != nil {
			return nil, fmt.Errorf("unmarshal message: %w", err)
		}
		message.Id = notification.ID
		message.CreatedAt = timestamppb.New(notification.CreatedAt)
		ns = append(ns, &message)
	}
	return ns, nil
}

func wrapError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func toTimestamp(t sql.NullTime) *timestamppb.Timestamp {
	if t.Valid {
		return timestamppb.New(t.Time)
	}
	return nil
}
