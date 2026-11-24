package web

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Money in the UI is formatted from integer cents, exactly as the
// ledger stores it — floats never appear. Display format:
// "SGD 1,234.56" (currency code, thousands separators, always two
// decimals); a negative amount leads with a minus: "−SGD 250.00".

// FormatMoney renders cents as "<CCY> d,ddd.dd".
func FormatMoney(currency string, cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := fmt.Sprintf("%s %s.%02d", currency, groupThousands(cents/100), cents%100)
	if neg {
		return "−" + s
	}
	return s
}

// FormatSigned is FormatMoney with an explicit + on positive
// amounts, for statement rows where direction matters at a glance.
func FormatSigned(currency string, cents int64) string {
	if cents > 0 {
		return "+" + FormatMoney(currency, cents)
	}
	return FormatMoney(currency, cents)
}

// groupThousands renders a non-negative integer with comma
// separators: 1234567 → "1,234,567".
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

var (
	errAmountShape = errors.New("amount must look like 12.34 — dollars with up to 2 decimal places")
	errAmountZero  = errors.New("amount must be more than zero")
)

// ParseAmountToCents converts a form amount in dollars ("12.34")
// into integer cents (1234) with strict validation: digits only, an
// optional single decimal point with 1–2 digits after it, no signs,
// no thousands separators, and the result must be positive. It
// parses the string digit by digit — never via a float — so "10.5"
// is exactly 1050 cents.
func ParseAmountToCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errAmountShape
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || !isDigits(parts[0]) {
		return 0, errAmountShape
	}
	if len(parts[0]) > 13 { // keep dollars*100 far from int64 overflow
		return 0, errAmountShape
	}
	dollars, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, errAmountShape
	}
	cents := dollars * 100
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) == 0 || len(frac) > 2 || !isDigits(frac) {
			return 0, errAmountShape
		}
		if len(frac) == 1 {
			frac += "0"
		}
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, errAmountShape
		}
		cents += f
	}
	if cents <= 0 {
		return 0, errAmountZero
	}
	return cents, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
