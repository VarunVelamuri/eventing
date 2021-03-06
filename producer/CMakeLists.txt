PROJECT (eventing-producer)

INCLUDE (FindCouchbaseGo)
INCLUDE (FindCouchbaseJemalloc)
INCLUDE (FindCouchbaseCurl)

SET(LDFLAGS)
IF(APPLE)
  SET (LDFLAGS "-s -extldflags '-Wl,-rpath,@executable_path/../lib'")
ENDIF()
IF(WIN32)
   SET (LDFLAGS "${LDFLAGS} -linkmode internal")
ENDIF()

IF(NOT DEFINED GOVERSION)
  MESSAGE(FATAL_ERROR "GOVERSION must be defined")
ENDIF()

SET(TAGS "jemalloc")
IF(BUILD_ENTERPRISE)
  SET(TAGS "${TAGS} enterprise")
ENDIF()

GET_FILENAME_COMPONENT(JEMALLOC_LIB_DIR ${JEMALLOC_LIBRARIES} DIRECTORY)
GET_FILENAME_COMPONENT (CURL_LIB_DIR "${CURL_LIBRARIES}" DIRECTORY)
SET(ENV{CGO_CFLAGS} "$ENV{CGO_CFLAGS} -DJEMALLOC=1")
SET(CGO_INCLUDE_DIRS "${sigar_SOURCE_DIR}/include;${Platform_SOURCE_DIR}/include;${JEMALLOC_INCLUDE_DIR};${CURL_INCLUDE_DIR}")
SET(CGO_LIBRARY_DIRS "${sigar_BINARY_DIR}/src;${Platform_BINARY_DIR};${JEMALLOC_LIB_DIR};${CURL_LIB_DIR}")

GoInstall(TARGET eventing-producer PACKAGE github.com/couchbase/eventing/cmd/producer
  GOPATH "${PROJECT_SOURCE_DIR}/../../../../.." "${GODEPSDIR}"
  INSTALL_PATH bin OUTPUT eventing-producer
  CGO_INCLUDE_DIRS "${CGO_INCLUDE_DIRS}"
  CGO_LIBRARY_DIRS "${CGO_LIBRARY_DIRS}"
  GOTAGS "${TAGS}"
  LDFLAGS "${LDFLAGS}"
  GOVERSION ${GOVERSION})

ADD_DEPENDENCIES(eventing-producer generated cbq-engine indexer)
