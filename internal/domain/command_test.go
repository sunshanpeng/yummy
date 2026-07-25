package domain

import "testing"

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name string
		from CommandStatus
		to   CommandStatus
		want bool
	}{
		{
			name: "新指令可以进入已下发",
			from: StatusCreated,
			to:   StatusDispatched,
			want: true,
		},
		{
			name: "已下发可以进入运行中",
			from: StatusDispatched,
			to:   StatusRunning,
			want: true,
		},
		{
			name: "运行中可以成功",
			from: StatusRunning,
			to:   StatusSucceeded,
			want: true,
		},
		{
			name: "终态不能倒退",
			from: StatusSucceeded,
			to:   StatusRunning,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Fatalf(
					"CanTransition(%q, %q) = %v，期望 %v",
					tt.from,
					tt.to,
					got,
					tt.want,
				)
			}
		})
	}
}
