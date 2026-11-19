package runtimeconfig

func (s *Service) validateFinalSize(workspaceID, applicationID string, revision int64, spec storedSpec) error {
	total := 0
	for _, group := range []struct {
		kind   string
		values []secretValue
		limit  int
	}{
		{"variable", spec.Variables, 65536}, {"file", spec.Files, 1 << 20},
	} {
		for _, value := range group.values {
			plain := []byte(value.Value)
			if value.Sensitive {
				var err error
				plain, err = s.box.Decrypt(value.Ciphertext, secretIdentity(workspaceID, applicationID, group.kind, value.Name, revision))
				if err != nil {
					return err
				}
			}
			if len(plain) > group.limit {
				return invalid("最终配置单项超过大小限制")
			}
			total += len(plain)
			if total > 2<<20 {
				return invalid("最终配置内容总量超过2MiB")
			}
		}
	}
	return nil
}
