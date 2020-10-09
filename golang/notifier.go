package xsuportal

import (
	"crypto/elliptic"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/golang/protobuf/proto"
	"github.com/jmoiron/sqlx"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/isucon/isucon10-final/webapp/golang/proto/xsuportal/resources"
)

const (
	WebpushVAPIDPrivateKeyPath = "../vapid_private.pem"
	WebpushSubject             = "xsuportal@example.com"
)

var webpushOptions *webpush.Options

func PreLoadVAPIDKey() {
	pemBytes, err := ioutil.ReadFile(WebpushVAPIDPrivateKeyPath)
	if err != nil {
		return
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return
	}
	priKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return
	}
	priBytes := priKey.D.Bytes()
	pubBytes := elliptic.Marshal(priKey.Curve, priKey.X, priKey.Y)
	pri := base64.RawURLEncoding.EncodeToString(priBytes)
	pub := base64.RawURLEncoding.EncodeToString(pubBytes)
	webpushOptions = &webpush.Options{
		Subscriber:      WebpushSubject,
		VAPIDPrivateKey: pri,
		VAPIDPublicKey:  pub,
	}
}

func VAPIDKey() *webpush.Options {
	return webpushOptions
}

func NotifyClarificationAnswered(db sqlx.Ext, c *Clarification, updated bool) error {
	var contestants []struct {
		ID     string `db:"id"`
		TeamID int64  `db:"team_id"`
	}
	if c.Disclosed.Valid && c.Disclosed.Bool {
		err := sqlx.Select(
			db,
			&contestants,
			"SELECT `id`, `team_id` FROM `contestants` WHERE `team_id` IS NOT NULL",
		)
		if err != nil {
			return fmt.Errorf("select all contestants: %w", err)
		}
	} else {
		err := sqlx.Select(
			db,
			&contestants,
			"SELECT `id`, `team_id` FROM `contestants` WHERE `team_id` = ?",
			c.TeamID,
		)
		if err != nil {
			return fmt.Errorf("select contestants(team_id=%v): %w", c.TeamID, err)
		}
	}
	for _, contestant := range contestants {
		notificationPB := &resources.Notification{
			Content: &resources.Notification_ContentClarification{
				ContentClarification: &resources.Notification_ClarificationMessage{
					ClarificationId: c.ID,
					Owned:           c.TeamID == contestant.TeamID,
					Updated:         updated,
				},
			},
		}
		notification, err := notify(db, notificationPB, contestant.ID)
		if err != nil {
			return fmt.Errorf("notify: %w", err)
		}
		if VAPIDKey() != nil {
			notificationPB.Id = notification.ID
			notificationPB.CreatedAt = timestamppb.New(notification.CreatedAt)

			sendNotificationByWebPush(db, contestant.ID, notificationPB)
		}
	}
	return nil
}

func NotifyBenchmarkJobFinished(db sqlx.Ext, job *BenchmarkJob) error {
	var contestants []struct {
		ID     string `db:"id"`
		TeamID int64  `db:"team_id"`
	}
	err := sqlx.Select(
		db,
		&contestants,
		"SELECT `id`, `team_id` FROM `contestants` WHERE `team_id` = ?",
		job.TeamID,
	)
	if err != nil {
		return fmt.Errorf("select contestants(team_id=%v): %w", job.TeamID, err)
	}
	for _, contestant := range contestants {
		notificationPB := &resources.Notification{
			Content: &resources.Notification_ContentBenchmarkJob{
				ContentBenchmarkJob: &resources.Notification_BenchmarkJobMessage{
					BenchmarkJobId: job.ID,
				},
			},
		}
		notification, err := notify(db, notificationPB, contestant.ID)
		if err != nil {
			return fmt.Errorf("notify: %w", err)
		}
		if VAPIDKey() != nil {
			notificationPB.Id = notification.ID
			notificationPB.CreatedAt = timestamppb.New(notification.CreatedAt)

			sendNotificationByWebPush(db, contestant.ID, notificationPB)
		}
	}
	return nil
}

func notify(db sqlx.Ext, notificationPB *resources.Notification, contestantID string) (*Notification, error) {
	m, err := proto.Marshal(notificationPB)
	if err != nil {
		return nil, fmt.Errorf("marshal notification: %w", err)
	}
	encodedMessage := base64.StdEncoding.EncodeToString(m)
	res, err := db.Exec(
		"INSERT INTO `notifications` (`contestant_id`, `encoded_message`, `read`, `created_at`, `updated_at`) VALUES (?, ?, FALSE, NOW(6), NOW(6))",
		contestantID,
		encodedMessage,
	)
	if err != nil {
		return nil, fmt.Errorf("insert notification: %w", err)
	}
	lastInsertID, _ := res.LastInsertId()
	var notification Notification
	err = sqlx.Get(
		db,
		&notification,
		"SELECT * FROM `notifications` WHERE `id` = ? LIMIT 1",
		lastInsertID,
	)
	if err != nil {
		return nil, fmt.Errorf("get inserted notification: %w", err)
	}
	return &notification, nil
}

func sendNotificationByWebPush(db sqlx.Ext, contestantId string, notificationPB *resources.Notification) error {
	var pushSubscription PushSubscription
	err := sqlx.Get(
		db,
		&pushSubscription,
		"SELECT * FROM `push_subscriptions` WHERE `contestant_id` = ? LIMIT 1",
		contestantId,
	)
	if err != nil {
		return fmt.Errorf("select push subscriptions: %w", err)
	}
	if err == sql.ErrNoRows {
		return nil
	}

	b, err := proto.Marshal(notificationPB)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	message := make([]byte, base64.StdEncoding.EncodedLen(len(b)))
	base64.StdEncoding.Encode(message, b)

	options := VAPIDKey()

	// Send Notification
	for i := 0; i < 2; i++ {
		resp, err := webpush.SendNotification(message,
			&webpush.Subscription{
				Endpoint: pushSubscription.Endpoint,
				Keys: webpush.Keys{
					Auth:   pushSubscription.Auth,
					P256dh: pushSubscription.P256DH,
				},
			},
			&webpush.Options{
				Subscriber:      options.Subscriber,
				VAPIDPublicKey:  options.VAPIDPublicKey,
				VAPIDPrivateKey: options.VAPIDPrivateKey,
				TTL:             30,
			})
		if err != nil {
			log.Printf("webpush SendNotification: %w", err)
			continue
			//	return fmt.Errorf("webpush SendNotification: %w", err)
		}
		defer resp.Body.Close()
	}
	return nil
}
