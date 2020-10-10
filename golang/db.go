package xsuportal

import (
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"github.com/isucon/isucon10-final/webapp/golang/util"
)

func GetDB() (*sqlx.DB, error) {
	mysqlConfig := mysql.NewConfig()
	mysqlConfig.Net = "tcp"
	mysqlConfig.Addr = util.GetEnv("MYSQL_HOSTNAME3", "10.162.47.101") + ":" + util.GetEnv("MYSQL_PORT", "3306")
	mysqlConfig.User = util.GetEnv("MYSQL_USER", "isucon")
	mysqlConfig.Passwd = util.GetEnv("MYSQL_PASS", "isucon")
	mysqlConfig.DBName = util.GetEnv("MYSQL_DATABASE", "xsuportal")
	mysqlConfig.Params = map[string]string{
		"time_zone": "'+00:00'",
		"interpolateParams": "true",
	}
	mysqlConfig.ParseTime = true

	return sqlx.Open("mysql", mysqlConfig.FormatDSN())
}
