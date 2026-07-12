package authsvc_test

import "math/big"

func allowNoCertificateRevocations(*big.Int, *big.Int) bool { return false }
