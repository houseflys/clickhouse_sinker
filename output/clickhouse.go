/*Copyright [2019] housepower

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package output

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gammazero/workerpool"
	"github.com/housepower/clickhouse_sinker/config"
	"github.com/housepower/clickhouse_sinker/input"
	"github.com/housepower/clickhouse_sinker/model"
	"github.com/housepower/clickhouse_sinker/pool"
	"github.com/housepower/clickhouse_sinker/statistics"
	"github.com/housepower/clickhouse_sinker/util"
	"github.com/pkg/errors"

	"github.com/housepower/clickhouse_sinker/prom"
	"github.com/sundy-li/go_commons/log"
	"github.com/sundy-li/go_commons/utils"
)

// ClickHouse is an output service consumers from kafka messages
type ClickHouse struct {
	Dims []*model.ColumnWithType
	// Table Configs
	taskCfg *config.TaskConfig
	chCfg   *config.ClickHouseConfig

	prepareSQL string
	dms        []string
	wp         *workerpool.WorkerPool
}

// NewClickHouse new a clickhouse instance
func NewClickHouse(taskCfg *config.TaskConfig) *ClickHouse {
	cfg := config.GetConfig()
	return &ClickHouse{taskCfg: taskCfg, chCfg: cfg.Clickhouse[taskCfg.Clickhouse]}
}

// Init the clickhouse intance
func (c *ClickHouse) Init() error {
	return c.initAll()
}

// Send a batch to clickhouse
func (c *ClickHouse) Send(batch input.Batch) {
	c.wp.Submit(func() {
		c.loopWrite(batch)
	})
}

// Write kvs to clickhouse
func (c *ClickHouse) write(batch input.Batch) (err error) {
	if len(batch.MsgRows) == 0 {
		return
	}

	conn := pool.GetConn(c.chCfg.Host)
	tx, err := conn.Begin()
	if err != nil {
		if shouldReconnect(err) {
			_ = conn.ReConnect()
		}
		return err
	}

	stmt, err := tx.Prepare(c.prepareSQL)
	if err != nil {
		log.Error("prepareSQL:", err.Error())

		if shouldReconnect(err) {
			_ = conn.ReConnect()
		}
		return err
	}

	defer stmt.Close()
	var numErr, numSuc int
	for _, msgRow := range batch.MsgRows {
		if msgRow.Row == nil {
			numErr++
		} else if _, err = stmt.Exec(msgRow.Row...); err != nil {
			err = errors.Wrap(err, "")
			numErr++
		} else {
			numSuc++
		}
	}
	prom.ClickhouseEventsTotal.WithLabelValues(c.chCfg.DB, c.taskCfg.TableName).Add(float64(numErr + numSuc))
	prom.ClickhouseEventsErrors.WithLabelValues(c.chCfg.DB, c.taskCfg.TableName).Add(float64(numErr))
	prom.ClickhouseEventsSuccess.WithLabelValues(c.chCfg.DB, c.taskCfg.TableName).Add(float64(numSuc))
	if err != nil {
		log.Errorf("stmt.Exec: %+v", err)
		return
	}
	if err = tx.Commit(); err != nil {
		err = errors.Wrap(err, "")
		if shouldReconnect(err) {
			_ = conn.ReConnect()
		}
		return err
	}
	err = batch.Free()
	return
}

func shouldReconnect(err error) bool {
	if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "bad connection") {
		return true
	}
	log.Info("not match reconnect rules", err.Error())
	return false
}

// LoopWrite will dead loop to write the records
func (c *ClickHouse) loopWrite(batch input.Batch) {
	err := c.write(batch)
	times := c.chCfg.RetryTimes
	if errors.Cause(err) == context.Canceled {
		log.Infof("ClickHouse.loopWrite quit due to the context has been cancelled")
		return
	}
	for err != nil && times > 0 {
		log.Error("saving msg error", err.Error(), "will loop to write the data")
		statistics.UpdateFlushErrorsTotal(c.taskCfg.Name, 1)
		time.Sleep(1 * time.Second)
		err = c.write(batch)
		times--
	}
}

// Close does nothing, place holder for handling close
func (c *ClickHouse) Close() error {
	c.wp.StopWait()
	return nil
}

// initAll initialises schema and connections for clickhouse
func (c *ClickHouse) initAll() error {
	if err := c.initConn(); err != nil {
		return err
	}
	if err := c.initSchema(); err != nil {
		return err
	}
	return nil
}

func (c *ClickHouse) initSchema() (err error) {
	if c.taskCfg.AutoSchema {
		conn := pool.GetConn(c.chCfg.Host)
		rs, err := conn.Query(fmt.Sprintf(
			"select name, type, default_kind from system.columns where database = '%s' and table = '%s'", c.chCfg.DB, c.taskCfg.TableName))
		if err != nil {
			return err
		}

		c.Dims = make([]*model.ColumnWithType, 0, 10)
		var name, typ, defaultKind string
		for rs.Next() {
			_ = rs.Scan(&name, &typ, &defaultKind)
			typ = lowCardinalityRegexp.ReplaceAllString(typ, "$1")
			if !util.StringContains(c.taskCfg.ExcludeColumns, name) && defaultKind != "MATERIALIZED" {
				c.Dims = append(c.Dims, &model.ColumnWithType{Name: name, Type: typ})
			}
		}
	} else {
		c.Dims = make([]*model.ColumnWithType, 0)
		for _, dim := range c.taskCfg.Dims {
			c.Dims = append(c.Dims, &model.ColumnWithType{
				Name:       dim.Name,
				Type:       dim.Type,
				SourceName: dim.SourceName,
			})
		}
	}
	//根据 dms 生成prepare的sql语句
	c.dms = make([]string, 0, len(c.Dims))
	for _, d := range c.Dims {
		c.dms = append(c.dms, d.Name)
	}
	var params = make([]string, len(c.Dims))
	for i := range params {
		params[i] = "?"
	}
	c.prepareSQL = "INSERT INTO " + c.chCfg.DB + "." + c.taskCfg.TableName + " (" + strings.Join(c.dms, ",") + ") " +
		"VALUES (" + strings.Join(params, ",") + ")"

	log.Info("Prepare sql=>", c.prepareSQL)
	return nil
}

func (c *ClickHouse) initConn() (err error) {
	var hosts []string

	// if contains ',', that means it's a ip list
	if strings.Contains(c.chCfg.Host, ",") {
		hosts = strings.Split(strings.TrimSpace(c.chCfg.Host), ",")
	} else {
		ips, err := utils.GetIp4Byname(c.chCfg.Host)
		if err != nil {
			// fallback to ip
			ips = []string{c.chCfg.Host}
		}
		for _, ip := range ips {
			hosts = append(hosts, fmt.Sprintf("%s:%d", ip, c.chCfg.Port))
		}
	}

	var dsn = fmt.Sprintf("tcp://%s?database=%s&username=%s&password=%s", hosts[0], c.chCfg.DB, c.chCfg.Username, c.chCfg.Password)
	if len(hosts) > 1 {
		otherHosts := hosts[1:]
		dsn += "&alt_hosts="
		dsn += strings.Join(otherHosts, ",")
		dsn += "&connection_open_strategy=random"
	}

	if c.chCfg.DsnParams != "" {
		dsn += "&" + c.chCfg.DsnParams
	}
	// dsn += "&debug=1"
	for i := 0; i < len(hosts); i++ {
		pool.SetDsn(c.chCfg.Host, dsn, time.Duration(c.chCfg.MaxLifeTime)*time.Second)
	}

	c.wp = workerpool.New(2 * len(hosts))
	return nil
}

var (
	lowCardinalityRegexp = regexp.MustCompile(`LowCardinality\((.+)\)`)
)
