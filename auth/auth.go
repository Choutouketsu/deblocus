package auth

import (
	"github.com/Lafeng/deblocus/exception"
	"strings"
)

var (
	NO_SUCH_USER          = exception.NewW("No such user")
	AUTH_FAILED           = exception.NewW("Auth failed")
	UNIMPLEMENTED_AUTHSYS = exception.NewW("Unimplemented authsys")
	INVALID_AUTH_CONF     = exception.NewW("Invalid Auth config")
	INVALID_AUTH_PARAMS   = exception.NewW("Invalid Auth params")
)

type AuthSys interface {
	Authenticate(user, passwd string) (bool, error)
	AddUser(user *User) error
	UserInfo(user string) (*User, error)
}

type User struct {
	Name string
	Pass string
}

func GetAuthSysImpl(proto string) (AuthSys, error) {
	sep := strings.Index(proto, "://")
	if sep > 0 {
		switch proto[:sep] {
		case "file":
			return NewFileAuthSys(proto[sep+3:])
		}
	}
	return nil, UNIMPLEMENTED_AUTHSYS.Apply("for " + proto)
}
