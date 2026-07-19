// Copyright 2025 The Kubernetes Authors.
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

package controllers

import (
	"os"

	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// adoptionTimingLog is a dedicated, never-sampled JSON logger for the
// one-line-per-adoption "adoption timing" summary.
//
// WHY IT EXISTS: controller-runtime's production zap configuration wraps its
// core in zapcore.NewSamplerWithOptions(core, 1s, 100, 100) — after 100
// occurrences of the SAME message within a sampler window, further lines are
// DROPPED (until every 100th). "adoption timing" is exactly such a message:
// during the round-3 leg-B 300-claim burst the controller logged ~2100
// lines/s at the peak and 76/300 timing lines were silently sampled away.
// Because sampling bites hardest when the most adoptions complete
// simultaneously, the surviving sample is biased (round-3: the missing
// cohort's e2e p50 was 1382ms vs 909ms for surviving lines) — every quantile
// computed from the log lines was quietly wrong.
//
// The sampler cannot be disabled from the outside: it is appended by
// controller-runtime's zap.Options defaulting as the outermost WrapCore, so
// this file gives the timing line its own zapcore with no sampler. The
// encoder mirrors the controller-runtime production JSON layout (level/ts/
// msg keys) so existing `grep '"adoption timing"' | jq` tooling keeps
// working, with one deliberate improvement: ts is RFC3339Nano, because the
// 1-second timestamps of the main log were a documented forensics pitfall.
//
// Emission is still gated on the main logger's V(1) enablement at the call
// site, so non-debug deployments emit nothing.
var adoptionTimingLog = newAdoptionTimingLog(zapcore.Lock(os.Stderr))

func newAdoptionTimingLog(ws zapcore.WriteSyncer) *uberzap.Logger {
	encCfg := uberzap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	return uberzap.New(zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), ws, uberzap.DebugLevel))
}
