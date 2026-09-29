// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package payments

import (
	"testing"

	"github.com/stripe/stripe-go/v79"
)

// The summary is printed on a customer's receipt, so each method type must
// read naturally, including ones that are not cards.
func TestSummarizePaymentMethod(t *testing.T) {
	cases := []struct {
		name string
		pm   *stripe.PaymentMethod
		want string
	}{
		{"visa card", &stripe.PaymentMethod{Card: &stripe.PaymentMethodCard{Brand: "visa", Last4: "4242"}}, "Visa ending 4242"},
		{"amex card", &stripe.PaymentMethod{Card: &stripe.PaymentMethodCard{Brand: "amex", Last4: "0005"}}, "American Express ending 0005"},
		{"unknown brand", &stripe.PaymentMethod{Card: &stripe.PaymentMethodCard{Brand: "cartes_bancaires", Last4: "1111"}}, "Cartes bancaires ending 1111"},
		{"ach bank", &stripe.PaymentMethod{USBankAccount: &stripe.PaymentMethodUSBankAccount{BankName: "STRIPE TEST BANK", Last4: "6789"}}, "STRIPE TEST BANK account ending 6789"},
		{"ach no bank name", &stripe.PaymentMethod{USBankAccount: &stripe.PaymentMethodUSBankAccount{Last4: "6789"}}, "Bank account ending 6789"},
		{"link", &stripe.PaymentMethod{Link: &stripe.PaymentMethodLink{}}, "Link"},
		{"other type", &stripe.PaymentMethod{Type: "revolut_pay"}, "revolut pay"},
		{"nothing known", &stripe.PaymentMethod{}, "Stripe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizePaymentMethod(tc.pm); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
