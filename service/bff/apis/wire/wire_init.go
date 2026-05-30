package wire

import (
	"github.com/google/wire"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	"gorm.io/gorm"
)

// ConfigSet provides infrastructure dependencies needed by the rule CRUD path.
// Redis was dropped in Task Q — both consumers of RuleStrategyUsecase are
// single-process and the in-process atomic.Pointer cache is sufficient.
var ConfigSet = wire.NewSet(provideGormDB)

func provideGormDB() *gorm.DB {
	return pkgdb.GetDB()
}
