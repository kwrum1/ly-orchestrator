cmake_minimum_required(VERSION 3.16)
project(ly-route-vpp-pppoeclient-hijiajun C)
set(CMAKE_LIBRARY_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}/lib)
include_directories(
  ${CMAKE_CURRENT_SOURCE_DIR}/pppoeclient
  ${CMAKE_BINARY_DIR}/src
)
find_package(VPP REQUIRED)
add_subdirectory(pppoeclient)
