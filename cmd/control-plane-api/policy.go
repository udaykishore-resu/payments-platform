package main

import (
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules/l2merchant"
)

// l2Deps is the L2 tenant-policy input, aliased so main.go names one type rather than a package
// path in three places.
type l2Deps = l2merchant.Deps
