package wire

import (
	"github.com/google/wire"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	"gorm.io/gorm"
)

var ConfigSet = wire.NewSet(provideGormDB)

func provideGormDB(conf *initialize.Conf) *gorm.DB {
	pkgdb.Init(conf.DB.DSN)
	return pkgdb.GetDB()
}
