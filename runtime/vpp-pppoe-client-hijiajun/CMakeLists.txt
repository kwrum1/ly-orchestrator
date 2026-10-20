cmake_minimum_required(VERSION 3.16)
project(ly-route-vpp-pppoeclient-hijiajun C)
set(CMAKE_LIBRARY_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
# Sources include headers as <pppoeclient/...> and <pppoeclient/pppox/...>,
# while the VPP-compat shim is included as <vlib/handoff.h>.  Keep both the
# plugin source root and its inner pppoeclient directory on the include path.
include_directories(
  ${CMAKE_CURRENT_SOURCE_DIR}
  ${CMAKE_CURRENT_SOURCE_DIR}/pppoeclient
  ${CMAKE_BINARY_DIR}
  ${CMAKE_BINARY_DIR}/src
)
find_package(VPP REQUIRED)
add_subdirectory(pppoeclient)
