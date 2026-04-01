package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/bff/worker"
)

var HandlerSet = wire.NewSet(worker.NewHandler)
