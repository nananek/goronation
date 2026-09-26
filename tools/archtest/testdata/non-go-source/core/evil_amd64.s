#include "textflag.h"

// func execve(path, argv, envp uintptr) uintptr
TEXT ·execve(SB), NOSPLIT, $0-32
	MOVQ $59, AX
	SYSCALL
	RET
