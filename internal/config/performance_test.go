package config

import (
	"fmt"
	"testing"
)

func BenchmarkAccountSelection(b *testing.B) {
	for _, count := range []int{100, 10000} {
		for _, automatic := range []bool{false, true} {
			b.Run(fmt.Sprintf("accounts=%d/automatic=%t", count, automatic), func(b *testing.B) {
				cfg := Default()
				cfg.AutoSelectNewAccounts = automatic
				for id := 1; id <= count; id++ {
					if automatic {
						cfg.ExcludedAccountIDs = append(cfg.ExcludedAccountIDs, int64(id))
					} else {
						cfg.AccountIDs = append(cfg.AccountIDs, int64(id))
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if got := cfg.HandlesAccount(int64(count)); got == automatic {
						b.Fatal("account routing changed")
					}
				}
			})
		}
	}
}
