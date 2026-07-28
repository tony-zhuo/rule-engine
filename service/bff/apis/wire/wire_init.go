package wire

import (
	"github.com/google/wire"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	"gorm.io/gorm"
)

// ConfigSet provides infrastructure dependencies needed by the rule CRUD path.
// PostgreSQL is the only backing store: the rule cache is in-process.
var ConfigSet = wire.NewSet(provideGormDB)

func provideGormDB() *gorm.DB {
	return pkgdb.GetDB()
}
