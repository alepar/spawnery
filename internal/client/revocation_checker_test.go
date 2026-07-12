package client

import "math/big"

func allowNoCertificateRevocations(*big.Int, *big.Int) bool { return false }
