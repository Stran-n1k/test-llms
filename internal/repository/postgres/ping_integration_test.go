package postgres_test

func (s *RepositorySuite) TestPing_SucceedsOnReachableDatabase() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	err := repository.Ping(s.T().Context())

	// Assert
	s.Require().NoError(err)
}

func (s *RepositorySuite) TestPing_FailsOnClosedPool() {
	// Arrange
	repository, pool := s.newRepository()
	pool.Close()

	// Act
	err := repository.Ping(s.T().Context())

	// Assert
	s.Require().Error(err)
}
