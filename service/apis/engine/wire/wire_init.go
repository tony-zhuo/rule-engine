package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	"gorm.io/gorm"
)

var ConfigSet = wire.NewSet(provideGormDB)

func provideGormDB(cfg *config.Config) *gorm.DB {
	pkgdb.Init(cfg.DB)
	return pkgdb.GetDB()
}
