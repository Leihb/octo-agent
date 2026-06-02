package fixture

// User is a person record serialized to JSON for the API.
type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}
