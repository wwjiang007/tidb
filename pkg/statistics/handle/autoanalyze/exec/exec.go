// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exec

import (
	"math"
	"strconv"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/planner/core/resolve"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/sysproctrack"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/statistics"
	statslogutil "github.com/pingcap/tidb/pkg/statistics/handle/logutil"
	statstypes "github.com/pingcap/tidb/pkg/statistics/handle/types"
	statsutil "github.com/pingcap/tidb/pkg/statistics/handle/util"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/sqlescape"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"go.uber.org/zap"
)

var execOptionForAnalyze = map[int]sqlexec.OptionFuncAlias{
	statistics.Version0: sqlexec.ExecOptionAnalyzeVer1,
	statistics.Version1: sqlexec.ExecOptionAnalyzeVer1,
	statistics.Version2: sqlexec.ExecOptionAnalyzeVer2,
}

// AutoAnalyze executes the auto analyze task.
func AutoAnalyze(
	sctx sessionctx.Context,
	statsHandle statstypes.StatsHandle,
	sysProcTracker sysproctrack.Tracker,
	statsVer int,
	sql string,
	params ...any,
) bool {
	startTime := time.Now()
	_, _, err := RunAnalyzeStmt(sctx, statsHandle, sysProcTracker, statsVer, sql, params...)
	dur := time.Since(startTime)
	metrics.AutoAnalyzeHistogram.Observe(dur.Seconds())
	if err != nil {
		escaped, err1 := sqlescape.EscapeSQL(sql, params...)
		if err1 != nil {
			escaped = ""
		}
		statslogutil.StatsErrVerboseSampleLogger().Error(
			"auto analyze failed",
			zap.String("sql", escaped),
			zap.Duration("cost_time", dur),
			zap.Error(err),
		)
		metrics.AutoAnalyzeCounter.WithLabelValues("failed").Inc()
		return false
	}
	metrics.AutoAnalyzeCounter.WithLabelValues("succ").Inc()
	return true
}

// RunAnalyzeStmt executes the analyze statement.
func RunAnalyzeStmt(
	sctx sessionctx.Context,
	statsHandle statstypes.StatsHandle,
	sysProcTracker sysproctrack.Tracker,
	statsVer int,
	sql string,
	params ...any,
) ([]chunk.Row, []*resolve.ResultField, error) {
	pruneMode := sctx.GetSessionVars().PartitionPruneMode.Load()
	analyzeSnapshot := sctx.GetSessionVars().EnableAnalyzeSnapshot
	autoAnalyzeTracker := statsutil.NewAutoAnalyzeTracker(sysProcTracker.Track, sysProcTracker.UnTrack)
	autoAnalyzeProcID := statsHandle.AutoAnalyzeProcID()
	optFuncs := []sqlexec.OptionFuncAlias{
		execOptionForAnalyze[statsVer],
		sqlexec.GetAnalyzeSnapshotOption(analyzeSnapshot),
		sqlexec.GetPartitionPruneModeOption(pruneMode),
		sqlexec.ExecOptionUseCurSession,
		sqlexec.ExecOptionWithSysProcTrack(autoAnalyzeProcID, autoAnalyzeTracker.Track, autoAnalyzeTracker.UnTrack),
	}
	defer func() {
		if r := recover(); r != nil {
			logutil.BgLogger().Warn("panic in execAnalyzeStmt", zap.Any("error", r), zap.Stack("stack"))
		}
		statsHandle.ReleaseAutoAnalyzeProcID(autoAnalyzeProcID)
	}()
	return statsutil.ExecWithOpts(sctx, optFuncs, sql, params...)
}

// GetAutoAnalyzeParameters gets the auto analyze parameters from mysql.global_variables.
func GetAutoAnalyzeParameters(sctx sessionctx.Context) map[string]string {
	sql := "select variable_name, variable_value from mysql.global_variables where variable_name in (%?, %?, %?)"
	rows, _, err := statsutil.ExecWithOpts(sctx, nil, sql, vardef.TiDBAutoAnalyzeRatio, vardef.TiDBAutoAnalyzeStartTime, vardef.TiDBAutoAnalyzeEndTime)
	if err != nil {
		return map[string]string{}
	}
	parameters := make(map[string]string, len(rows))
	for _, row := range rows {
		parameters[row.GetString(0)] = row.GetString(1)
	}
	return parameters
}

// ParseAutoAnalyzeRatio parses the auto analyze ratio from the string.
func ParseAutoAnalyzeRatio(ratio string) float64 {
	autoAnalyzeRatio, err := strconv.ParseFloat(ratio, 64)
	if err != nil {
		return vardef.DefAutoAnalyzeRatio
	}
	return math.Max(autoAnalyzeRatio, 0)
}

// ParseAutoAnalysisWindow parses the time window for auto analysis.
// It parses the times in UTC location.
func ParseAutoAnalysisWindow(start, end string) (_, _ time.Time, err error) {
	if start == "" {
		start = vardef.DefAutoAnalyzeStartTime
	}
	if end == "" {
		end = vardef.DefAutoAnalyzeEndTime
	}
	s, err := time.ParseInLocation(vardef.FullDayTimeFormat, start, time.UTC)
	if err != nil {
		return s, s, errors.Trace(err)
	}
	e, err := time.ParseInLocation(vardef.FullDayTimeFormat, end, time.UTC)
	return s, e, err
}
