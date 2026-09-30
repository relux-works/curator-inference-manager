package infra

func sharedRuntimeError(code string, err error) error {
	return &SharedRuntimeError{Code: code, Err: err}
}
