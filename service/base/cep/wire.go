package cep

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/base/cep/model"
	"github.com/tony-zhuo/rule-engine/service/base/cep/usecase"
)

var CEPUsecaseSet = wire.NewSet(
	usecase.NewCEPUsecase,
	wire.Bind(new(model.ProcessorInterface), new(*usecase.CEPUsecase)),
)

var MockCEPProvider = wire.NewSet(CEPUsecaseSet)
